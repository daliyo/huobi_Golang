package websocketclientbase

import (
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/huobirdcenter/huobi_golang/internal/gzip"
	"github.com/huobirdcenter/huobi_golang/internal/model"
	"github.com/huobirdcenter/huobi_golang/internal/requestbuilder"
	"github.com/huobirdcenter/huobi_golang/pkg/response/auth"
	"github.com/huobirdcenter/huobi_golang/pkg/response/base"
	"sync"
	"time"
)

const (
	websocketV2Path = "/ws/v2"
)

// It will be invoked after websocket v2 authentication response received
type AuthenticationV2ResponseHandler func(resp *auth.WebSocketV2AuthenticationResponse)

// The base class that responsible to get data from websocket authentication v2
type WebSocketV2ClientBase struct {
	host string
	conn *websocket.Conn

	authenticationResponseHandler AuthenticationV2ResponseHandler
	messageHandler                MessageHandler
	responseHandler               ResponseHandler

	stopReadChannel   chan int
	stopTickerChannel chan int
	ticker            *time.Ticker
	lastReceivedTime  time.Time
	sendMutex         *sync.Mutex

	requestBuilder *requestbuilder.WebSocketV2RequestBuilder
}

// Initializer
func (p *WebSocketV2ClientBase) Init(accessKey string, secretKey string, host string) *WebSocketV2ClientBase {
	p.host = host
	p.stopReadChannel = make(chan int, 1)
	p.stopTickerChannel = make(chan int, 1)
	p.requestBuilder = new(requestbuilder.WebSocketV2RequestBuilder).Init(accessKey, secretKey, host, websocketV2Path)
	p.sendMutex = &sync.Mutex{}
	return p
}

// Set callback handler
func (p *WebSocketV2ClientBase) SetHandler(authHandler AuthenticationV2ResponseHandler, msgHandler MessageHandler, repHandler ResponseHandler) {
	p.authenticationResponseHandler = authHandler
	p.messageHandler = msgHandler
	p.responseHandler = repHandler
}

// Connect to websocket server
// if autoConnect is true, then the connection can be re-connect if no data received after the pre-defined timeout
func (p *WebSocketV2ClientBase) Connect(autoConnect bool) error {
	err := p.connectWebSocket()
	if err != nil {
		return err
	}

	if autoConnect {
		p.startTicker()
	}

	return nil
}

// Send data to websocket server
func (p *WebSocketV2ClientBase) Send(data string) error {
	if p.conn == nil {
		return errors.New("no connection available")
	}

	p.sendMutex.Lock()
	err := p.conn.WriteMessage(websocket.TextMessage, []byte(data))
	p.sendMutex.Unlock()
	return err
}

// Close the connection to server
func (p *WebSocketV2ClientBase) Close() {
	p.stopTicker()
	p.disconnectWebSocket()
}

// connect to server
func (p *WebSocketV2ClientBase) connectWebSocket() error {
	var err error
	url := fmt.Sprintf("wss://%s%s", p.host, websocketV2Path)
	fmt.Println("WebSocket connecting...")
	p.conn, _, err = websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return err
	}
	fmt.Println("WebSocket connected")

	auth, err := p.requestBuilder.Build()
	if err != nil {
		return err
	}

	err = p.Send(auth)
	if err != nil {
		return err
	}

	p.startReadLoop()

	return nil
}

// disconnect with server
func (p *WebSocketV2ClientBase) disconnectWebSocket() {
	if p.conn == nil {
		return
	}

	p.stopReadLoop()

	fmt.Println("WebSocket disconnecting...")
	err := p.conn.Close()
	if err != nil {
		fmt.Printf("WebSocket disconnect error: %s\n", err)
		return
	}

	fmt.Println("WebSocket disconnected")
}

// initialize a ticker and start a goroutine tickerLoop()
func (p *WebSocketV2ClientBase) startTicker() {
	p.ticker = time.NewTicker(TimerIntervalSecond * time.Second)
	p.lastReceivedTime = time.Now()

	go p.tickerLoop()
}

// stop ticker and stop the goroutine
func (p *WebSocketV2ClientBase) stopTicker() {
	p.ticker.Stop()
	p.stopTickerChannel <- 1
}

// defines a for loop that will run based on ticker's frequency
// It checks the last data that received from server, if it is longer than the threshold,
// it will force disconnect server and connect again.
func (p *WebSocketV2ClientBase) tickerLoop() {
	fmt.Println("tickerLoop started")
	for {
		select {
		// start a goroutine readLoop()
		case <-p.stopTickerChannel:
			fmt.Println("tickerLoop stopped")
			return

		// Receive tick from tickChannel
		case <-p.ticker.C:
			elapsedSecond := time.Now().Sub(p.lastReceivedTime).Seconds()
			fmt.Printf("WebSocket received data %f sec ago\n", elapsedSecond)

			if elapsedSecond > ReconnectWaitSecond {
				fmt.Println("WebSocket reconnect...")
				p.disconnectWebSocket()
				err := p.connectWebSocket()
				if err != nil {
					fmt.Printf("WebSocket reconnect error: %s\n", err)
				}
			}
		}
	}
}

// start a goroutine readLoop()
func (p *WebSocketV2ClientBase) startReadLoop() {
	go p.readLoop()
}

// stop the goroutine readLoop()
func (p *WebSocketV2ClientBase) stopReadLoop() {
	p.stopReadChannel <- 1 //TODO: consider put this into goroutine to unblock
}

// defines a for loop to read data from server
// it will stop once it receives the signal from stopReadChannel
func (p *WebSocketV2ClientBase) readLoop() {
	fmt.Println("readLoop started")
	for {
		select {
		// Receive data from stopChannel
		case <-p.stopReadChannel:
			fmt.Println("readLoop stopped")
			return

		default:
			if p.conn == nil {
				fmt.Printf("Read error: no connection available")
				time.Sleep(TimerIntervalSecond * time.Second)
				continue
			}

			msgType, buf, err := p.conn.ReadMessage()
			if err != nil {
				fmt.Printf("Read error: %s\n", err)
				time.Sleep(TimerIntervalSecond * time.Second)
				continue
			}

			p.lastReceivedTime = time.Now()

			// decompress gzip data if it is binary message
			var message string
			if msgType == websocket.BinaryMessage {
				message, err = gzip.GZipDecompress(buf)
				if err != nil {
					fmt.Printf("UnGZip data error: %s\n", err)
				}
			} else if msgType == websocket.TextMessage {
				message = string(buf)
			}

			// Try to pass as PingV2Message
			// If it is Ping then respond Pong
			pingV2Msg := model.ParsePingV2Message(message)
			if pingV2Msg.IsPing() {
				fmt.Printf("Received Ping: %d\n", pingV2Msg.Data.Timestamp)
				pongMsg := fmt.Sprintf("{\"action\": \"pong\", \"data\": { \"ts\": %d } }", pingV2Msg.Data.Timestamp)
				p.Send(pongMsg)
				fmt.Printf("Respond  Pong: %d\n", pingV2Msg.Data.Timestamp)
			} else {
				// Try to pass as websocket v2 authentication response
				// If it is then invoke authentication handler
				wsV2Resp := base.ParseWSV2Resp(message)
				if wsV2Resp != nil {
					switch wsV2Resp.Action {
					case "req":
						authResp := auth.ParseWSV2AuthResp(message)
						if authResp != nil && p.authenticationResponseHandler != nil {
							p.authenticationResponseHandler(authResp)
						}

					case "sub", "push":
						{
							result, err := p.messageHandler(message)
							if err != nil {
								fmt.Printf("Handle message error: %s\n", err)
								continue
							}
							if p.responseHandler != nil {
								p.responseHandler(result)
							}
						}
					}
				}
			}
		}
	}
}
