package fluent

import (
	"github.com/streadway/amqp"
	"time"
	"context"
	"net"
	"regexp"
	"sync"
)

var maskPassword = regexp.MustCompile(`[^/:]+:([^@:]+)@`)

type StateHandler interface {
	ChannelReady(ctx context.Context, ch *amqp.Channel) error
}

type Server struct {
	config          BrokerConfig
	handlersLock    sync.Mutex
	handlers        []StateHandler
	refreshHandlers chan struct{}
	done            chan struct{}
	urlIndex        int
}

func (brk *Server) handle(st StateHandler) *Server {
	brk.handlersLock.Lock()
	brk.handlers = append(brk.handlers, st)
	brk.handlersLock.Unlock()
	select {
	case brk.refreshHandlers <- struct{}{}:
	default:

	}
	return brk
}

func (brk *Server) Sink(queueName string) *SinkConfig {
	return newSink(queueName, brk)
}

func (brk *Server) Publisher() *WriterConfig {
	return newWriter(brk)
}

func (brk *Server) Requeue(originalQueue string) *ReQueueConfig {
	return newRequeue(brk, originalQueue)
}

func (brk *Server) WaitToFinish() {
	<-brk.done
}

func (brk *Server) nextURL() string {
	u := brk.config.urls[brk.urlIndex]
	brk.urlIndex = (brk.urlIndex + 1) % len(brk.config.urls)
	return u
}

func (brk *Server) serve() error {
	defer close(brk.done)
	for {
		url := brk.nextURL()
		brk.config.logger.Println("connecting to", maskPassword.ReplaceAllString(url, "***@"))
		conn, err := amqp.DialConfig(url, amqp.Config{
			Heartbeat: 10 * time.Second,
			Locale:    "en_US",
			Dial: func(network, addr string) (net.Conn, error) {
				d := net.Dialer{
					KeepAlive: 10 * time.Second,
				}
				cctx, closer := context.WithTimeout(brk.config.ctx, brk.config.connectTimeout)
				defer closer()
				return d.DialContext(cctx, network, addr)
			},
		})
		if err != nil {
			brk.config.logger.Println("connection error:", err)
		} else {
			brk.config.logger.Println("successfully connected to", maskPassword.ReplaceAllString(url, "***@"))
			brk.processConnection(conn)
			conn.Close()
		}
		brk.config.logger.Println("try to reconnect after", brk.config.reconnectInterval)
		select {
		case <-time.After(brk.config.reconnectInterval):
		case <-brk.config.ctx.Done():
			brk.config.logger.Println("reconnect aborted due to context close")
			return brk.config.ctx.Err()
		}
	}
}

func (brk *Server) processConnection(conn *amqp.Connection) {
	childContext, closer := context.WithCancel(brk.config.ctx)
	wg := sync.WaitGroup{}
	loaded := brk.loadHandler(conn, childContext, closer, &wg, 0)
LOOP:
	for {
		select {
		case <-childContext.Done():
			break LOOP
		case <-brk.refreshHandlers:
			// load delta (if exists)
			loaded += brk.loadHandler(conn, childContext, closer, &wg, loaded)
		}
	}
	wg.Wait()
}

func (brk *Server) loadHandler(conn *amqp.Connection, childContext context.Context, closer context.CancelFunc, wg *sync.WaitGroup, offset int) int {
	brk.handlersLock.Lock()
	var loaded int
	for _, handler := range brk.handlers[offset:] {
		wg.Add(1)
		go func(handler StateHandler) {
			defer wg.Done()
			err := brk.runHandler(childContext, conn, handler)
			if err != nil {
				brk.config.logger.Println("failed handler:", err)
			}
			closer()
		}(handler)
		loaded++
	}
	brk.handlersLock.Unlock()
	return loaded
}

func (brk *Server) runHandler(ctx context.Context, conn *amqp.Connection, handler StateHandler) error {
	ch, err := conn.Channel()
	if err != nil {
		brk.config.logger.Println("failed open channel:", err)
		return err
	}
	defer ch.Close()
	err = ch.Qos(1, 0, true)
	if err != nil {
		brk.config.logger.Println("failed set QoS:", err)
		return err
	}
	return handler.ChannelReady(ctx, ch)
}
