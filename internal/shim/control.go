package shim

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"time"
)

// Request is a command to a shim.
type Request struct {
	Token string `json:"token"`
	// Op is "status", "signal" (Signal names it), "restart" or "stop".
	Op     string `json:"op"`
	Signal string `json:"signal,omitempty"`
}

// Response is a shim's answer.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	State State  `json:"state"`
}

// Call sends req to the shim at addr and waits up to timeout for its answer.
func Call(addr string, req Request, timeout time.Duration) (Response, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, err
	}
	var res Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&res); err != nil {
		return Response{}, err
	}
	if !res.OK {
		return res, errors.New(res.Error)
	}
	return res, nil
}

// serve answers control connections until the listener closes.
func (s *supervisor) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		s.handlers.Add(1)
		go func() {
			defer s.handlers.Done()
			defer conn.Close()
			s.handle(conn)
		}()
	}
}

func (s *supervisor) handle(conn net.Conn) {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var req Request
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&req); err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})
	enc := json.NewEncoder(conn)
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.spec.Token)) != 1 {
		enc.Encode(Response{Error: "unauthorized"})
		return
	}
	switch req.Op {
	case "status":
		enc.Encode(Response{OK: true, State: s.current()})
	case "signal", "restart", "stop":
		reply := make(chan Response, 1)
		select {
		case s.requests <- request{Request: req, reply: reply}:
			enc.Encode(<-reply)
		case <-s.done:
			enc.Encode(Response{OK: true, State: s.current()})
		}
	default:
		enc.Encode(Response{Error: "unknown op " + req.Op})
	}
}
