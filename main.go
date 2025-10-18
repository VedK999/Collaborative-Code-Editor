package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

const (
	MessageTypeConnected        = "connected"
	MessageTypeJoinCodeSpace    = "join_codespace"
	MessageTypeCodeSync         = "code_sync"
	MessageTypeCodeChange       = "code_change"
	MessageTypeCodeUpdate       = "code_update"
	MessageTypeCursorUpdate     = "cursor_update"
	MessageTypeExecuteCode      = "execute_code"
	MessageTypeExecutionResult  = "execution_result"
	MessageTypeExecutionStarted = "execution_started"
	MessageTypeLanguageChange   = "language_change"
	MessageTypeUserJoined       = "user_joined"
	MessageTypeUserLeft         = "user_left"
	MessageTypeError            = "error"
	MessageTypePing             = "ping"
	MessageTypePong             = "pong"
)

// websocket message
type Message struct {
	Type        string                 `json:"type"`
	UserID      string                 `json:"userId,omitempty"`
	CodeSpaceID string                 `json:"codeSpaceId,omitempty"`
	Code        string                 `json:"code,omitempty"`
	Language    string                 `json:"language,omitempty"`
	Cursor      *CursorPosition        `json:"cursor,omitempty"`
	Users       []string               `json:"users,omitempty"`
	Message     string                 `json:"message,omitempty"`
	Success     bool                   `json:"success,omitempty"`
	Output      string                 `json:"output,omitempty"`
	Stderr      string                 `json:"stderr,omitempty"`
	ExecTime    int64                  `json:"executionTime,omitempty"`
	MemoryUsage int64                  `json:"memoryUsage,omitempty"`
	Error       string                 `json:"error,omitempty"`
	Timestamp   time.Time              `json:"timestamp,omitempty"`
	Data        map[string]interface{} `json:"data,omitempty"`
}

type CursorPosition struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

type User struct {
	ID           string
	Conn         *websocket.Conn
	CodeSpaceID  string
	JoinedAt     time.Time
	LastActivity time.Time
	Send         chan Message
	mu           sync.RWMutex
}

type CodeSpace struct {
	ID           string
	Code         string
	Language     string
	Users        map[string]*User
	CreatedAt    time.Time
	LastModified time.Time
	mu           sync.RWMutex
}

type Server struct {
	codeSpaces    map[string]*CodeSpace
	users         map[string]*User
	redisManager  *RedisManager
	dockerManager *DockerManager
	upgrader      websocket.Upgrader
	mu            sync.RWMutex
	ctx           context.Context
	cancel        context.CancelFunc
}

func NewServer() *Server {
	ctx, cancel := context.WithCancel(context.Background())

	return &Server{
		codeSpaces: make(map[string]*CodeSpace),
		users:      make(map[string]*User),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},

		ctx:    ctx,
		cancel: cancel,
	}
}

func (s *Server) Initialize() error {
	redisManger, err := NewRedisManager("localhost:6379", "", 0)
	if err != nil {
		return err
	}
	s.redisManager = redisManger

	dockerManager, err := NewDockerManager()
	if err != nil {
		return err
	}
	s.dockerManager = dockerManager

	go s.handleRedisSubscriptions()

	go s.periodicCleanup()

	log.Println("Server initialized successfully")

	return nil
}

func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Websocket upgrade error: %v", err)
		return
	}

	userID := uuid.New().String()
	user := &User{
		ID:           userID,
		Conn:         conn,
		JoinedAt:     time.Now(),
		LastActivity: time.Now(),
		Send:         make(chan Message, 256),
	}

	s.mu.Lock()
	s.users[userID] = user
	s.mu.Unlock()

	log.Printf("User connected: %s from %s", userID, r.RemoteAddr)

	welcomeMsg := Message{
		Type:      MessageTypeConnected,
		UserID:    userID,
		Timestamp: time.Now(),
	}

	user.Send <- welcomeMsg

	go s.writePump(user)
	go s.readPump(user)
}

func (s *Server) readPump(user *User) {
	defer func() {
		s.handleDisconnect(user)
		user.Conn.Close()
	}()

	user.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	user.Conn.SetPongHandler(func(string) error {
		user.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, messageData, err := user.Conn.ReadMessage()

		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Websocket error for user %s: %v", user.ID, err)
			}

			break
		}

		var msg Message
		if err := json.Unmarshal(messageData, &msg); err != nil {
			log.Printf("JSON unmarhsal error: %v", err)
			user.Send <- Message{
				Type:    MessageTypeError,
				Message: "Invalid message format",
			}

			continue
		}

		user.mu.Lock()
		user.LastActivity = time.Now()
		user.mu.Unlock()

		go s.handleMessage(user, &msg)
	}
}

func (s *Server) writePump(user *User) {
	ticker := time.NewTicker(54 * time.Second)
	defer func() {
		ticker.Stop()
		user.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-user.Send:
			user.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				user.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			data, err := json.Marshal(message)
			if err != nil {
				log.Printf("JSON marshal error: %v", err)
				continue
			}

			if err := user.Conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}

		case <-ticker.C:
			user.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := user.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		}
	}
}

func (s *Server) handleMessage(user *User, msg *Message) {
	switch msg.Type {
	case MessageTypeJoinCodeSpace:
		s.handleJoinCodeSpace(user, msg.CodeSpaceID)
	case MessageTypeCodeChange:
		s.handleCodeChange(user, msg)
	case MessageTypeCursorUpdate:
		s.handleCursorUpdate(user, msg)
	case MessageTypeExecuteCode:
		s.handleExecuteCode(user)
	case MessageTypeLanguageChange:
		s.handleLanguageChange(user, msg.Language)
	case MessageTypePing:
		user.Send <- Message{Type: MessageTypePong}
	default:
		log.Printf("Unknown message type: %s from user %s", msg.Type, user.ID)
	}
}

func (s *Server) handleJoinCodeSpace(user *User, codeSpaceID string) {
	if user.CodeSpaceID != "" {
		s.leaveCodeSpace(user)
	}

	s.mu.Lock()
	codeSpace, exists := s.codeSpaces[codeSpaceID]
	if !exists {
		savedCodeSpace, err := s.redisManager.GetCodeSpace(codeSpaceID)
		if err == nil && savedCodeSpace != nil {
			codeSpace = savedCodeSpace
			s.codeSpaces[codeSpaceID] = codeSpace
		} else {
			codeSpace = &CodeSpace{
				ID:           codeSpaceID,
				Code:         s.dockerManager.GetCodeTemplate("javascript"),
				Language:     "javascript",
				Users:        make(map[string]*User),
				CreatedAt:    time.Now(),
				LastModified: time.Now(),
			}
			s.codeSpaces[codeSpaceID] = codeSpace
		}
	}
	s.mu.Unlock()

	codeSpace.mu.Lock()
	codeSpace.Users[user.ID] = user
	users := s.getCodeSpaceUserIDs(codeSpace)
	codeSpace.mu.Unlock()

	user.mu.Lock()
	user.CodeSpaceID = codeSpaceID
	user.mu.Unlock()

	user.Send <- Message{
		Type:      MessageTypeCodeSync,
		Code:      codeSpace.Code,
		Language:  codeSpace.Language,
		Users:     users,
		Timestamp: time.Now(),
	}

	s.broadcastToCodeSpace(codeSpaceID, Message{
		Type:      MessageTypeUserJoined,
		UserID:    user.ID,
		Users:     users,
		Timestamp: time.Now(),
	}, user.ID)

	s.redisManager.PublishUserJoined(codeSpaceID, user.ID)
	s.redisManager.AddUserToCodeSpace(codeSpaceID, user.ID)

	log.Printf("User %s joined code space %s", user.ID, codeSpaceID)
}

func (s *Server) handleCodeChange(user *User, msg *Message) {
	if user.CodeSpaceID == "" {
		return
	}

	s.mu.RLock()
	codeSpace, exists := s.codeSpaces[user.CodeSpaceID]
	s.mu.RUnlock()

	if !exists {
		return
	}

	codeSpace.mu.Lock()
	codeSpace.Code = msg.Code
	codeSpace.LastModified = time.Now()
	codeSpace.mu.Unlock()

	broadCastMsg := Message{
		Type:      MessageTypeCodeUpdate,
		Code:      msg.Code,
		UserID:    user.ID,
		Timestamp: time.Now(),
	}

	s.broadcastToCodeSpace(user.CodeSpaceID, broadCastMsg, user.ID)
	s.redisManager.PublishCodeChange(user.CodeSpaceID, msg.Code, user.ID)

	go s.redisManager.SaveCodeSpace(codeSpace)
}

func (s *Server) handleCursorUpdate(user *User, msg *Message) {
	if user.CodeSpaceID == "" {
		return
	}

	broadcastMsg := Message{
		Type:      MessageTypeCursorUpdate,
		UserID:    user.ID,
		Cursor:    msg.Cursor,
		Timestamp: time.Now(),
	}

	s.broadcastToCodeSpace(user.CodeSpaceID, broadcastMsg, user.ID)
	s.redisManager.PublishCursorUpdate(user.CodeSpaceID, user.ID, msg.Cursor)
}

func (s *Server) handleExecuteCode(user *User) {
	if user.CodeSpaceID == "" {
		user.Send <- Message{
			Type:    MessageTypeError,
			Message: "Not in a code space",
		}

		return
	}

	s.mu.RLock()
	codeSpace, exists := s.codeSpaces[user.CodeSpaceID]
	s.mu.RUnlock()

	if !exists {
		return
	}

	allowed, err := s.redisManager.CheckRateLimit(user.ID, "execute", 5, 60)
	if err != nil || !allowed {
		user.Send <- Message{
			Type:    MessageTypeExecutionResult,
			Success: false,
			Error:   "Rate Limit exceeded. Please wait before executing code again.",
		}

		return
	}

	user.Send <- Message{
		Type:    MessageTypeExecutionStarted,
		Message: "Executing code...",
	}

	result, err := s.dockerManager.ExecuteCode(codeSpace.Code, codeSpace.Language, user.ID)

	if err != nil {
		user.Send <- Message{
			Type:    MessageTypeExecutionResult,
			Success: false,
			Error:   err.Error(),
		}
		return
	}

	user.Send <- Message{
		Type:        MessageTypeExecutionResult,
		Success:     result.Success,
		Output:      result.Output,
		Stderr:      result.Stderr,
		ExecTime:    result.ExecutionTime,
		MemoryUsage: result.MemoryUsage,
	}

	s.broadcastToCodeSpace(user.CodeSpaceID, Message{
		Type:     "code_executed",
		UserID:   user.ID,
		Success:  result.Success,
		ExecTime: result.ExecutionTime,
	}, user.ID)
}

func (s *Server) handleLanguageChange(user *User, language string) {
	if user.CodeSpaceID == "" {
		return
	}

	s.mu.RLock()
	codeSpace, exists := s.codeSpaces[user.CodeSpaceID]
	s.mu.RUnlock()

	if !exists {
		return
	}

	codeSpace.mu.Lock()
	codeSpace.Language = language
	codeSpace.LastModified = time.Now()
	codeSpace.mu.Unlock()

	s.broadcastToCodeSpace(user.CodeSpaceID, Message{
		Type:      MessageTypeLanguageChange,
		Language:  language,
		UserID:    user.ID,
		Timestamp: time.Now(),
	}, "")

	s.redisManager.PublishLanguageChange(user.CodeSpaceID, language, user.ID)
	go s.redisManager.SaveCodeSpace(codeSpace)
}

func (s *Server) broadcastToCodeSpace(codeSpaceID string, msg Message, excludeUserID string) {
	s.mu.RLock()
	codeSpace, exists := s.codeSpaces[codeSpaceID]
	s.mu.RUnlock()

	if !exists {
		return
	}

	codeSpace.mu.RLock()
	defer codeSpace.mu.RUnlock()

	for userID, user := range codeSpace.Users {
		if userID != excludeUserID {
			select {
			case user.Send <- msg:
			default:
				log.Printf("Skipping message to user %s (channel full)", userID)
			}
		}
	}
}

func (s *Server) leaveCodeSpace(user *User) {
	if user.CodeSpaceID == "" {
		return
	}

	s.mu.RLock()
	codeSpace, exists := s.codeSpaces[user.CodeSpaceID]
	s.mu.RUnlock()

	if !exists {
		return
	}

	codeSpace.mu.Lock()
	delete(codeSpace.Users, user.ID)
	users := s.getCodeSpaceUserIDs(codeSpace)
	codeSpace.mu.Unlock()

	s.broadcastToCodeSpace(user.CodeSpaceID, Message{
		Type:      MessageTypeUserLeft,
		UserID:    user.ID,
		Users:     users,
		Timestamp: time.Now(),
	}, "")

	s.redisManager.RemoveUserFromCodeSpace(user.CodeSpaceID, user.ID)
	s.redisManager.PublishUserLeft(user.CodeSpaceID, user.ID)

	user.mu.Lock()
	user.CodeSpaceID = ""
	user.mu.Unlock()
}

func (s *Server) handleDisconnect(user *User) {
	s.leaveCodeSpace(user)

	s.mu.Lock()
	delete(s.users, user.ID)
	s.mu.Unlock()

	close(user.Send)
	log.Printf("User disconnected: %s", user.ID)
}

func (s *Server) getCodeSpaceUserIDs(codeSpace *CodeSpace) []string {
	users := make([]string, 0, len(codeSpace.Users))
	for userID := range codeSpace.Users {
		users = append(users, userID)
	}

	return users
}

func (s *Server) handleRedisSubscriptions() {
	s.redisManager.Subscribe(s.ctx, func(channel string, message []byte) {
		parts := strings.Split(channel, ":")
		if len(parts) < 3 {
			log.Panicf("Invalid channel format: %s", channel)
		}

		resourceType := parts[0]
		resourceID := parts[1]
		eventType := parts[2]

		var msg Message
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("Error parsing Redis message: %v", err)
			return
		}

		switch resourceType {
		case "codespace":
			s.handelCodeSpaceRedisEvent(resourceID, eventType, &msg)
		case "execution":
			s.handleExecutionRedisEvent(resourceID, eventType, &msg)
		case "user":
			s.handleUserRedisEvent(resourceID, eventType, &msg)
		default:
			log.Printf("Unknown resource type in channel: %s", resourceType)
		}
	})
}

func (s *Server) handelCodeSpaceRedisEvent(codeSpaceID, eventType string, msg *Message) {
	s.mu.RLock()
	codeSpace, exists := s.codeSpaces[codeSpaceID]
	s.mu.RUnlock()

	if !exists {
		return
	}

	switch eventType {
	case "code":
		codeSpace.mu.Lock()
		codeSpace.Code = msg.Code
		codeSpace.LastModified = time.Now()
		codeSpace.mu.Unlock()

		s.broadcastToCodeSpace(codeSpaceID, *msg, msg.UserID)
		log.Printf("Redis: Code update in space %s from user %s", codeSpaceID, msg.UserID)

	case "cursor":
		s.broadcastToCodeSpace(codeSpaceID, *msg, msg.UserID)

	case "language":
		codeSpace.mu.Lock()
		codeSpace.Language = msg.Language
		codeSpace.LastModified = time.Now()
		codeSpace.mu.Unlock()

		s.broadcastToCodeSpace(codeSpaceID, *msg, msg.UserID)
		log.Printf("Redis: Language changed in space %s to %s", codeSpaceID, msg.Language)
	}
}

func (s *Server) handleExecutionRedisEvent(executionID, eventType string, msg *Message) {
	switch eventType {
	case "result":
		log.Printf("Redis: Execution %s completed with success=%v", executionID, msg.Success)

	case "error":
		log.Printf("Redis: Execution %s failed: %s", executionID, msg.Error)

	default:
		log.Printf("Unknown execution event type: %s", eventType)
	}
}

func (s *Server) handleUserRedisEvent(userID, eventType string, msg *Message) {
	switch eventType {
	case "session":
		log.Printf("Redis: user %s session updated", userID)

	default:
		log.Printf("Unknown user event typr: %s", eventType)
	}
}

func (s *Server) periodicCleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.cleanup()
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Server) cleanup() {
	now := time.Now()
	inactiveThreshold := 1 * time.Hour

	s.mu.Lock()
	defer s.mu.Unlock()

	// Clean up empty code spaces
	for id, cs := range s.codeSpaces {
		cs.mu.RLock()
		userCount := len(cs.Users)
		lastMod := cs.LastModified
		cs.mu.RUnlock()

		if userCount == 0 && now.Sub(lastMod) > inactiveThreshold {
			delete(s.codeSpaces, id)
			go s.redisManager.DeleteCodeSpace(id)
			log.Printf("Cleaned up inactive code space: %s", id)
		}
	}

	log.Printf("Cleanup completed. Active users: %d, Active code spaces: %d",
		len(s.users), len(s.codeSpaces))
}

func (s *Server) Shutdown() {
	log.Println("Shutting down server...")

	s.cancel()

	s.mu.RLock()
	for _, user := range s.users {
		user.Conn.Close()
	}
	s.mu.RUnlock()

	if s.dockerManager != nil {
		s.dockerManager.Cleanup()
	}

	if s.redisManager != nil {
		s.redisManager.Close()
	}

	log.Println("Server shutdown complete")
}

func (s *Server) HandleCreateCodeSpace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Language string `json:"language"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.Language = "javascript"
	}

	codeSpaceID := uuid.New().String()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"codeSpaceID": codeSpaceID,
		"language":    req.Language,
	})

	log.Printf("Created code space: %s", codeSpaceID)
}

func (s *Server) HandleGetCodeSpace(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	codeSpaceID := vars["id"]

	s.mu.RLock()
	codeSpace, exists := s.codeSpaces[codeSpaceID]
	s.mu.RUnlock()

	if !exists {
		savedCodeSpace, err := s.redisManager.GetCodeSpace(codeSpaceID)
		if err != nil || savedCodeSpace == nil {
			http.Error(w, "Code space not found", http.StatusNotFound)
			return
		}
		codeSpace = savedCodeSpace
	}

	codeSpace.mu.RLock()
	defer codeSpace.mu.RLock()

	response := map[string]interface{}{
		"id":           codeSpaceID,
		"code":         codeSpace.Code,
		"language":     codeSpace.Language,
		"users":        s.getCodeSpaceUserIDs(codeSpace),
		"createdAt":    codeSpace.CreatedAt,
		"lastModified": codeSpace.LastModified,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *Server) HandleHealth(w http.ResponseWriter, r *http.Request) {
	health := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now(),
		"stats": map[string]int{
			"activeConnections": len(s.users),
			"activeCodeSpaces":  len(s.codeSpaces),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(health)
}

func main() {
	server := NewServer()

	if err := server.Initialize(); err != nil {
		log.Fatalf("Failed to initialize server: %v", err)
	}

	router := mux.NewRouter()

	router.HandleFunc("/ws", server.HandleWebSocket)
	router.HandleFunc("/api/codespace", server.HandleCreateCodeSpace).Methods("POST")
	router.HandleFunc("/api/codespace/{id}", server.HandleGetCodeSpace).Methods("POST")
	router.HandleFunc("/health", server.HandleHealth).Methods("GET")

	router.PathPrefix("/").Handler(http.FileServer(http.Dir("./public")))

	httpServer := &http.Server{
		Addr:         ":3000",
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	done := make(chan bool)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-quit
		log.Println("Server is shutting down...")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		httpServer.SetKeepAlivesEnabled(false)
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Fatalf("Could not gracefully shutdown the server: %v\n", err)
		}

		server.Shutdown()
		close(done)
	}()

	log.Printf("Server starting on :3000")
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server could not listen on :3000 %v\n", err)
	}

	<-done
	log.Println("Server stopped")

}
