package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dmz006/claude-signal/internal/router"
	"github.com/dmz006/claude-signal/internal/session"
)

// Server holds all HTTP handler dependencies
type Server struct {
	hub      *Hub
	manager  *session.Manager
	hostname string
	token    string
}

func NewServer(hub *Hub, manager *session.Manager, hostname, token string) *Server {
	return &Server{hub: hub, manager: manager, hostname: hostname, token: token}
}

// authMiddleware checks the Bearer token if one is configured
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			next.ServeHTTP(w, r)
			return
		}
		// Check Authorization header or ?token= query param
		tok := r.URL.Query().Get("token")
		if tok == "" {
			auth := r.Header.Get("Authorization")
			tok = strings.TrimPrefix(auth, "Bearer ")
		}
		if tok != s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleSessions returns all sessions as JSON
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	sessions := s.manager.ListSessions()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sessions) //nolint:errcheck
}

// handleSessionOutput returns the last N lines of a session's output
func (s *Server) handleSessionOutput(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	n := 50
	fmt.Sscanf(r.URL.Query().Get("n"), "%d", &n) //nolint:errcheck
	output, err := s.manager.TailOutput(id, n)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(output)) //nolint:errcheck
}

// handleCommand processes a command string (same format as Signal commands)
func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	cmd := router.Parse(req.Text)
	result := s.executeCommand(cmd, req.Text)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"result": result}) //nolint:errcheck
}

// executeCommand runs a parsed command and returns a response string
func (s *Server) executeCommand(cmd router.Command, raw string) string {
	switch cmd.Type {
	case router.CmdNew:
		if cmd.Text == "" {
			return "Usage: new: <task>"
		}
		sess, err := s.manager.Start(context.Background(), cmd.Text, "")
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		// Broadcast updated session list
		go s.hub.BroadcastSessions(s.manager.ListSessions())
		return fmt.Sprintf("[%s][%s] Started: %s\nTmux: %s", s.hostname, sess.ID, cmd.Text, sess.TmuxSession)

	case router.CmdList:
		sessions := s.manager.ListSessions()
		if len(sessions) == 0 {
			return "No active sessions."
		}
		var sb strings.Builder
		for _, sess := range sessions {
			sb.WriteString(fmt.Sprintf("[%s] %s — %s\n  %s\n", sess.ID, sess.State, sess.UpdatedAt.Format("15:04:05"), truncate(sess.Task, 60)))
		}
		return sb.String()

	case router.CmdStatus:
		output, err := s.manager.TailOutput(cmd.SessionID, 20)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return output

	case router.CmdSend:
		err := s.manager.SendInput(cmd.SessionID, cmd.Text)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		go s.hub.BroadcastSessions(s.manager.ListSessions())
		return fmt.Sprintf("[%s] Input sent.", cmd.SessionID)

	case router.CmdKill:
		err := s.manager.Kill(cmd.SessionID)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		go s.hub.BroadcastSessions(s.manager.ListSessions())
		return fmt.Sprintf("[%s] Killed.", cmd.SessionID)

	case router.CmdTail:
		n := cmd.TailN
		if n == 0 {
			n = 20
		}
		output, err := s.manager.TailOutput(cmd.SessionID, n)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return output

	case router.CmdAttach:
		sess, ok := s.manager.GetSession(cmd.SessionID)
		if !ok {
			return "Session not found."
		}
		return fmt.Sprintf("tmux attach -t %s", sess.TmuxSession)

	case router.CmdHelp:
		return router.HelpText(s.hostname)

	default:
		_ = raw // suppress unused variable warning
		return "Unknown command. Send 'help' for available commands."
	}
}

// handleWS upgrades a connection to WebSocket and registers it with the hub
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	c := &client{
		hub:        s.hub,
		conn:       conn,
		send:       make(chan []byte, 256),
		subscribed: make(map[string]bool),
	}
	s.hub.register <- c

	// Send initial session list
	sessions := s.manager.ListSessions()
	raw, _ := json.Marshal(SessionsData{Sessions: sessions})
	msg := WSMessage{Type: MsgSessions, Data: raw, Timestamp: time.Now()}
	payload, _ := json.Marshal(msg)
	c.send <- payload

	go c.writePump()

	// Read pump (blocking)
	defer func() {
		s.hub.unregister <- c
		conn.Close()
	}()

	conn.SetReadLimit(32 * 1024)
	conn.SetReadDeadline(time.Now().Add(60 * time.Second)) //nolint:errcheck
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second)) //nolint:errcheck
		return nil
	})

	for {
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			break
		}
		conn.SetReadDeadline(time.Now().Add(60 * time.Second)) //nolint:errcheck

		var inMsg WSMessage
		if err := json.Unmarshal(msgBytes, &inMsg); err != nil {
			continue
		}

		switch inMsg.Type {
		case MsgCommand:
			var d CommandData
			json.Unmarshal(inMsg.Data, &d) //nolint:errcheck
			cmd := router.Parse(d.Text)
			result := s.executeCommand(cmd, d.Text)
			// Send result back to this client
			respRaw, _ := json.Marshal(NotificationData{Message: result})
			resp := WSMessage{Type: MsgNotification, Data: respRaw, Timestamp: time.Now()}
			respPayload, _ := json.Marshal(resp)
			c.send <- respPayload

		case MsgNewSession:
			var d NewSessionData
			json.Unmarshal(inMsg.Data, &d) //nolint:errcheck
			cmd := router.Command{Type: router.CmdNew, Text: d.Task}
			result := s.executeCommand(cmd, "new: "+d.Task)
			respRaw, _ := json.Marshal(NotificationData{Message: result})
			resp := WSMessage{Type: MsgNotification, Data: respRaw, Timestamp: time.Now()}
			respPayload, _ := json.Marshal(resp)
			c.send <- respPayload

		case MsgSendInput:
			var d SendInputData
			json.Unmarshal(inMsg.Data, &d) //nolint:errcheck
			cmd := router.Command{Type: router.CmdSend, SessionID: d.SessionID, Text: d.Text}
			result := s.executeCommand(cmd, "")
			respRaw, _ := json.Marshal(NotificationData{Message: result})
			resp := WSMessage{Type: MsgNotification, Data: respRaw, Timestamp: time.Now()}
			respPayload, _ := json.Marshal(resp)
			c.send <- respPayload

		case MsgSubscribe:
			var d SubscribeData
			json.Unmarshal(inMsg.Data, &d) //nolint:errcheck
			c.mu.Lock()
			c.subscribed[d.SessionID] = true
			c.mu.Unlock()
			// Send recent output immediately
			output, err := s.manager.TailOutput(d.SessionID, 50)
			if err == nil {
				lines := strings.Split(output, "\n")
				outRaw, _ := json.Marshal(OutputData{SessionID: d.SessionID, Lines: lines})
				outMsg := WSMessage{Type: MsgOutput, Data: outRaw, Timestamp: time.Now()}
				outPayload, _ := json.Marshal(outMsg)
				c.send <- outPayload
			}

		case MsgPing:
			pongRaw, _ := json.Marshal(map[string]string{"type": "pong"})
			c.send <- pongRaw
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
