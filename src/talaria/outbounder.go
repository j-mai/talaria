package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/Comcast/webpa-common/device"
	"github.com/Comcast/webpa-common/logging"
	"github.com/Comcast/webpa-common/wrp"
	"github.com/spf13/viper"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"time"
)

const (
	// OutbounderKey is the Viper subkey which is expected to hold Outbounder configuration
	OutbounderKey = "device.outbound"

	// OutboundContentType is the Content-Type header value for device messages leaving talaria
	OutboundContentType = "application/wrp"

	EventPrefix = "event:"
	URLPrefix   = "url:"

	DefaultMethod                            = "POST"
	DefaultEventEndpoint                     = "http://localhost:8090/api/v2/notify"
	DefaultAssumeScheme                      = "https"
	DefaultAllowedScheme                     = "https"
	DefaultWorkerPoolSize                    = 100
	DefaultRequestQueueSize                  = 1000
	DefaultRequestTimeout      time.Duration = 5 * time.Second
	DefaultClientTimeout       time.Duration = 3 * time.Second
	DefaultEncoderPoolSize                   = 100
	DefaultMessageFailedSource               = "talaria"
	DefaultMessageFailedHeader               = "X-Webpa-Message-Delivery-Failure"
	DefaultMaxIdleConns                      = 0
	DefaultMaxIdleConnsPerHost               = 100
	DefaultIdleConnTimeout     time.Duration = 0
)

// RequestFactory is a simple function type for creating an outbound HTTP request
// for a given WRP message.
type requestFactory func(device.Interface, wrp.Routable, []byte) (*http.Request, error)

// listener is the internal device Listener type that dispatches requests over HTTP.
type listener struct {
	logger              logging.Logger
	requests            chan<- *http.Request
	requestFactory      requestFactory
	messageFailedHeader string
	encoderPool         *wrp.EncoderPool
}

func (l *listener) messageReceived(device device.Interface, message wrp.Routable, encoded []byte) {
	request, err := l.requestFactory(device, message, encoded)
	if err != nil {
		l.logger.Error("Unable to create request for device [%s]: %s", device.ID(), err)
		return
	}

	select {
	case l.requests <- request:
	default:
		l.logger.Error("Dropping outbound message for device [%s]: %s->%s", device.ID(), message.From(), message.To())
	}
}

func (l *listener) messageFailed(device device.Interface, message wrp.Routable, encoded []byte, sendError error) {
	if message.MessageType() == wrp.SimpleEventMessageType {
		return
	}

	var (
		failureResponse = message.Response("TODO", 1)
		encodedFailure  bytes.Buffer
	)

	if err := l.encoderPool.Encode(&encodedFailure, failureResponse); err != nil {
		l.logger.Error("Could not encode returned message for device [%s]: %s", device.ID(), err)
		return
	}

	request, err := l.requestFactory(device, failureResponse, encodedFailure.Bytes())
	if err != nil {
		l.logger.Error("Unable to create returned message request for device [%s]: %s", device.ID(), err)
		return
	}

	if sendError != nil {
		request.Header.Set(l.messageFailedHeader, sendError.Error())
	} else {
		request.Header.Set(l.messageFailedHeader, "Disconnected")
	}

	select {
	case l.requests <- request:
	default:
		l.logger.Error("Dropping returned message for device [%s]: %s->%s", device.ID(), failureResponse.From(), failureResponse.To())
	}
}

func (l *listener) OnDeviceEvent(event *device.Event) {
	switch event.Type {
	case device.MessageReceived:
		l.messageReceived(event.Device, event.Message, event.Encoded)
	case device.MessageFailed:
		l.messageFailed(event.Device, event.Message, event.Encoded, event.Err)
	}
}

// workerPool describes a pool of goroutines that dispatch http.Request objects to
// a transactor function
type workerPool struct {
	logger     logging.Logger
	requests   <-chan *http.Request
	transactor func(*http.Request) (*http.Response, error)
}

func (wp *workerPool) worker() {
	for request := range wp.requests {
		response, err := wp.transactor(request)
		if err != nil {
			wp.logger.Error("HTTP error: %s", err)
			continue
		}

		if response.StatusCode < 400 {
			wp.logger.Debug("HTTP response status: %s", response.Status)
		} else {
			wp.logger.Error("HTTP response status: %s", response.Status)
		}

		io.Copy(ioutil.Discard, response.Body)
		response.Body.Close()
	}
}

func (wp *workerPool) run(workers int) {
	for repeat := 0; repeat < workers; repeat++ {
		go wp.worker()
	}
}

// Outbounder acts as a configurable endpoint for dispatching WRP messages from devices
// and handling any failed messages.
type Outbounder struct {
	Method              string
	EventEndpoint       string
	DeviceNameHeader    string
	AssumeScheme        string
	AllowedSchemes      []string
	WorkerPoolSize      int
	RequestQueueSize    int
	RequestTimeout      time.Duration
	ClientTimeout       time.Duration
	EncoderPoolSize     int
	MessageFailedSource string
	MessageFailedHeader string
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
	Logger              logging.Logger
}

// NewOutbounder returns an Outbounder unmarshalled from a Viper environment.
// This function allows the Viper instance to be nil, in which case a default
// Outbounder is returned.
func NewOutbounder(logger logging.Logger, v *viper.Viper) (o *Outbounder, err error) {
	o = &Outbounder{
		Method:              DefaultMethod,
		EventEndpoint:       DefaultEventEndpoint,
		DeviceNameHeader:    device.DefaultDeviceNameHeader,
		AssumeScheme:        DefaultAssumeScheme,
		AllowedSchemes:      []string{DefaultAllowedScheme},
		WorkerPoolSize:      DefaultWorkerPoolSize,
		RequestQueueSize:    DefaultRequestQueueSize,
		RequestTimeout:      DefaultRequestTimeout,
		ClientTimeout:       DefaultClientTimeout,
		EncoderPoolSize:     DefaultEncoderPoolSize,
		MessageFailedSource: DefaultMessageFailedSource,
		MaxIdleConns:        DefaultMaxIdleConns,
		MaxIdleConnsPerHost: DefaultMaxIdleConnsPerHost,
		IdleConnTimeout:     DefaultIdleConnTimeout,
		Logger:              logger,
	}

	if v != nil {
		err = v.Unmarshal(o)
	}

	return
}

// newRoundTripper creates an HTTP RoundTripper (transport) using this Outbounder's configuration.
func (o *Outbounder) newRoundTripper() http.RoundTripper {
	return &http.Transport{
		MaxIdleConns:        o.MaxIdleConns,
		MaxIdleConnsPerHost: o.MaxIdleConnsPerHost,
		IdleConnTimeout:     o.IdleConnTimeout,
	}
}

// newTransactor returns a closure which can execute HTTP transactions
func (o *Outbounder) newTransactor() func(*http.Request) (*http.Response, error) {
	client := &http.Client{
		Transport: o.newRoundTripper(),
		Timeout:   o.ClientTimeout,
	}

	return client.Do
}

// newRequestFactory produces a RequestFactory function that creates an outbound HTTP request
// for a given WRP message from a specific device.
func (o *Outbounder) newRequestFactory() requestFactory {
	allowedSchemes := make(map[string]bool, len(o.AllowedSchemes))
	for _, scheme := range o.AllowedSchemes {
		allowedSchemes[scheme] = true
	}

	return func(device device.Interface, message wrp.Routable, encoded []byte) (r *http.Request, err error) {
		destination := message.To()
		if strings.HasPrefix(destination, EventPrefix) {
			// route this to the configured endpoint that receives all events
			r, err = http.NewRequest(o.Method, o.EventEndpoint, bytes.NewBuffer(encoded))
		} else if strings.HasPrefix(destination, URLPrefix) {
			// route this to the given URL, subject to some validation
			if r, err = http.NewRequest(o.Method, destination[len(URLPrefix):], bytes.NewBuffer(encoded)); err == nil {
				if len(r.URL.Scheme) == 0 {
					// if no scheme is supplied, use the configured AssumeScheme
					r.URL.Scheme = o.AssumeScheme
				} else if !allowedSchemes[r.URL.Scheme] {
					err = fmt.Errorf("Scheme not allowed: %s", r.URL.Scheme)
				}
			}
		} else {
			err = fmt.Errorf("Bad WRP destination: %s", destination)
		}

		if err != nil {
			return
		}

		r.Header.Set(o.DeviceNameHeader, string(device.ID()))
		r.Header.Set("Content-Type", OutboundContentType)
		// TODO: Need to set Convey?

		ctx, _ := context.WithTimeout(context.Background(), o.RequestTimeout)
		r = r.WithContext(ctx)

		return
	}
}

func (o *Outbounder) Start() device.Listener {
	var (
		requests = make(chan *http.Request, o.RequestQueueSize)

		workerPool = workerPool{
			logger:     o.Logger,
			requests:   requests,
			transactor: o.newTransactor(),
		}

		listener = &listener{
			logger:              o.Logger,
			requestFactory:      o.newRequestFactory(),
			messageFailedHeader: o.MessageFailedHeader,
			requests:            requests,
			encoderPool:         wrp.NewEncoderPool(o.EncoderPoolSize, wrp.Msgpack),
		}
	)

	workerPool.run(o.WorkerPoolSize)
	return listener.OnDeviceEvent
}