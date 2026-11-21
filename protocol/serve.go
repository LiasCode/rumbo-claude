package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// Invoker is what a provider that describes functions also implements. The
// result is sent to the caller as JSON.
type Invoker interface {
	Invoke(ctx context.Context, name string, params json.RawMessage) (any, error)
}

// DialogInvoker is what a provider implements whose functions talk with the
// person while they work, through dialog. Serve uses it instead of Invoker
// when a provider has both.
type DialogInvoker interface {
	InvokeDialog(ctx context.Context, name string, params json.RawMessage, dialog Dialog) (any, error)
}

// Handler is what a provider implements. Serve turns it into a protocol
// server, so a provider written in Go needs nothing else.
type Handler interface {
	Initialize(ctx context.Context, params InitializeParams) (InitializeResult, error)
	Complete(ctx context.Context, params CompleteParams, emit func(text string)) (CompleteResult, error)
}

// Serve speaks the protocol on r and w until the host sends shutdown or
// closes the stream. What is still running then is cancelled and waited
// for.
func Serve(ctx context.Context, r io.Reader, w io.Writer, handler Handler) error {
	server := &server{
		handler: handler,
		out:     json.NewEncoder(w),
		running: map[int64]context.CancelFunc{},
		asked:   map[int64]chan Frame{},
	}

	reader := bufio.NewReader(r)

	for {
		line, err := reader.ReadBytes('\n')

		if len(line) > 0 {
			var frame Frame

			if json.Unmarshal(line, &frame) == nil {
				switch {
				case frame.Method != "":
					if server.dispatch(ctx, frame) {
						server.stop()
						return nil
					}

				// The host answers a question a function asked.
				case frame.ID != nil:
					server.answered(frame)
				}
			}
		}

		if err != nil {
			server.stop()

			if errors.Is(err, io.EOF) {
				return nil
			}

			return err
		}
	}
}

type server struct {
	handler Handler
	wait    sync.WaitGroup

	mu      sync.Mutex
	out     *json.Encoder
	running map[int64]context.CancelFunc
	// asked are the questions sent to the host and not answered yet, by the
	// id of their request.
	asked  map[int64]chan Frame
	nextID int64
}

// dispatch handles one incoming frame and reports whether to shut down.
func (s *server) dispatch(ctx context.Context, frame Frame) bool {
	switch frame.Method {
	case MethodInitialize:
		var params InitializeParams

		if !s.decode(frame, &params) {
			return false
		}

		result, err := s.handler.Initialize(ctx, params)
		s.reply(frame.ID, result, err)

	case MethodInvoke:
		var params InvokeParams

		if !s.decode(frame, &params) || frame.ID == nil {
			return false
		}

		s.invoke(ctx, *frame.ID, params)

	case MethodComplete:
		var params CompleteParams

		if !s.decode(frame, &params) || frame.ID == nil {
			return false
		}

		id := *frame.ID

		s.background(ctx, id, func(ctx context.Context) (any, error) {
			emit := func(text string) {
				raw, _ := json.Marshal(DeltaParams{ID: id, Text: text})
				s.send(Frame{Method: MethodDelta, Params: raw})
			}

			return s.handler.Complete(ctx, params, emit)
		})

	case MethodCancel:
		var params CancelParams

		if json.Unmarshal(frame.Params, &params) == nil {
			s.mu.Lock()
			cancel := s.running[params.ID]
			s.mu.Unlock()

			if cancel != nil {
				cancel()
			}
		}

	case MethodShutdown:
		s.reply(frame.ID, struct{}{}, nil)
		return true

	default:
		if frame.ID != nil {
			s.send(Frame{ID: frame.ID, Error: &Error{Code: CodeMethodNotFound, Message: "unknown method: " + frame.Method}})
		}
	}

	return false
}

// invoke calls a function, with a dialog when the provider's functions talk
// with the person.
func (s *server) invoke(ctx context.Context, id int64, params InvokeParams) {
	s.background(ctx, id, func(ctx context.Context) (any, error) {
		if talker, able := s.handler.(DialogInvoker); able {
			return talker.InvokeDialog(ctx, params.Name, params.Params, dialog{server: s, id: id})
		}

		if invoker, able := s.handler.(Invoker); able {
			return invoker.Invoke(ctx, params.Name, params.Params)
		}

		return nil, &Error{Code: CodeMethodNotFound, Message: "this provider has no functions"}
	})
}

// background answers a request in a goroutine of its own, so a cancel, or
// the answer to a question, can still arrive while it works.
func (s *server) background(ctx context.Context, id int64, work func(ctx context.Context) (any, error)) {
	runCtx, cancel := context.WithCancel(ctx)

	s.mu.Lock()
	s.running[id] = cancel
	s.mu.Unlock()

	s.wait.Go(func() {
		defer cancel()

		result, err := work(runCtx)

		s.mu.Lock()
		delete(s.running, id)
		s.mu.Unlock()

		s.reply(&id, result, err)
	})
}

// stop cancels what is still running, so a function waiting for an answer
// that will not come ends, and waits for all of it.
func (s *server) stop() {
	s.mu.Lock()

	for _, cancel := range s.running {
		cancel()
	}

	s.mu.Unlock()
	s.wait.Wait()
}

// answered passes the host's answer to the question it belongs to.
func (s *server) answered(frame Frame) {
	s.mu.Lock()
	answer := s.asked[*frame.ID]
	delete(s.asked, *frame.ID)
	s.mu.Unlock()

	if answer != nil {
		answer <- frame
	}
}

// dialog is the Dialog of one function: what it says goes to the host as
// deltas of its invoke, and what it asks as ask requests.
type dialog struct {
	server *server
	id     int64
}

func (d dialog) Say(text string) {
	raw, _ := json.Marshal(DeltaParams{ID: d.id, Text: text})
	d.server.send(Frame{Method: MethodDelta, Params: raw})
}

func (d dialog) Ask(ctx context.Context, question string, secret bool) (string, error) {
	s := d.server
	answer := make(chan Frame, 1)

	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.asked[id] = answer
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.asked, id)
		s.mu.Unlock()
	}()

	raw, _ := json.Marshal(AskParams{ID: d.id, Text: question, Secret: secret})
	s.send(Frame{ID: &id, Method: MethodAsk, Params: raw})

	select {
	case <-ctx.Done():
		return "", ctx.Err()

	case frame := <-answer:
		if frame.Error != nil {
			return "", frame.Error
		}

		var result AskResult

		if err := json.Unmarshal(frame.Result, &result); err != nil {
			return "", err
		}

		return result.Text, nil
	}
}

func (s *server) decode(frame Frame, params any) bool {
	if err := json.Unmarshal(frame.Params, params); err != nil {
		if frame.ID != nil {
			s.send(Frame{ID: frame.ID, Error: &Error{Code: CodeInvalidParams, Message: err.Error()}})
		}

		return false
	}

	return true
}

func (s *server) reply(id *int64, result any, err error) {
	if id == nil {
		return
	}

	// A handler picks the code by returning an *Error, as Transient does.

	if failure, ok := errors.AsType[*Error](err); ok {
		s.send(Frame{ID: id, Error: failure})
		return
	}

	if err != nil {
		s.send(Frame{ID: id, Error: &Error{Code: CodeInternal, Message: err.Error()}})
		return
	}

	raw, err := json.Marshal(result)

	if err != nil {
		s.send(Frame{ID: id, Error: &Error{Code: CodeInternal, Message: err.Error()}})
		return
	}

	s.send(Frame{ID: id, Result: raw})
}

func (s *server) send(frame Frame) {
	frame.JSONRPC = "2.0"

	s.mu.Lock()
	defer s.mu.Unlock()

	// A failed write means the host is gone; the read loop ends on its own.
	_ = s.out.Encode(frame)
}
