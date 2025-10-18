package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/go-redis/redis/v8"
)

type RedisManager struct {
	client       *redis.Client
	pubSubClient *redis.Client
	ctx          context.Context
}

func NewRedisManager(addr, password string, db int) (*RedisManager, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	pubSubClient := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	ctx := context.Background()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis connection failed: %w", err)
	}

	log.Println("Redis connected successfully")

	return &RedisManager{
		client:       client,
		pubSubClient: pubSubClient,
		ctx:          ctx,
	}, nil
}

func (rm *RedisManager) SaveCodeSpace(cs *CodeSpace) error {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	data := map[string]interface{}{
		"id":           cs.ID,
		"code":         cs.Code,
		"language":     cs.Language,
		"createdAt":    cs.CreatedAt,
		"lastModified": cs.LastModified,
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	key := fmt.Sprintf("codespace:%s:data", cs.ID)
	return rm.client.Set(rm.ctx, key, jsonData, 24*time.Hour).Err()

}

func (rm *RedisManager) GetCodeSpace(codeSpaceID string) (*CodeSpace, error) {
	key := fmt.Sprintf("codespace:%s:data", codeSpaceID)
	data, err := rm.client.Get(rm.ctx, key).Result()

	if err == redis.Nil {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var csData struct {
		ID           string    `json:"id"`
		Code         string    `json:"code"`
		Language     string    `json:"language"`
		CreatedAt    time.Time `json:"createdat"`
		LastModified time.Time `json:"lastmodified"`
	}

	if err := json.Unmarshal([]byte(data), &csData); err != nil {
		return nil, err
	}

	return &CodeSpace{
		ID:           csData.ID,
		Code:         csData.Code,
		Language:     csData.Language,
		Users:        make(map[string]*User),
		CreatedAt:    csData.CreatedAt,
		LastModified: csData.LastModified,
	}, nil
}

func (rm *RedisManager) DeleteCodeSpace(codeSpaceID string) error {
	keys := []string{
		fmt.Sprintf("codespace:%s:data", codeSpaceID),
		fmt.Sprintf("codespace:%s:users", codeSpaceID),
	}
	return rm.client.Del(rm.ctx, keys...).Err()
}

func (rm *RedisManager) AddUserToCodeSpace(codeSpaceID, userID string) error {
	key := fmt.Sprintf("codespace:%s:users", codeSpaceID)
	err := rm.client.SAdd(rm.ctx, key, userID).Err()

	if err != nil {
		return err
	}

	return rm.client.Expire(rm.ctx, key, 1*time.Hour).Err()
}

func (rm *RedisManager) RemoveUserFromCodeSpace(codeSpaceID, userID string) error {
	key := fmt.Sprintf("codespace:%s:users", codeSpaceID)
	return rm.client.SRem(rm.ctx, key, userID).Err()
}

func (rm *RedisManager) GetCodeSpaceUsers(codeSpaceID string) ([]string, error) {
	key := fmt.Sprintf("codespace:%s:users", codeSpaceID)
	return rm.client.SMembers(rm.ctx, key).Result()
}

func (rm *RedisManager) PublishCodeChange(codeSpaceID, code, userID string) error {
	channel := fmt.Sprintf("codespace:%s:code", codeSpaceID)
	data := map[string]interface{}{
		"type":      "code_update",
		"code":      code,
		"userID":    userID,
		"timestamp": time.Now(),
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	return rm.client.Publish(rm.ctx, channel, jsonData).Err()
}

func (rm *RedisManager) PublishCursorUpdate(codeSpaceID, userID string, cursor *CursorPosition) error {
	channel := fmt.Sprintf("codespace:%s:cursor", codeSpaceID)
	data := map[string]interface{}{
		"type":      "cursor_update",
		"userId":    userID,
		"cursor":    cursor,
		"timestamp": time.Now(),
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	return rm.client.Publish(rm.ctx, channel, jsonData).Err()
}

func (rm *RedisManager) PublishUserJoined(codeSpaceID, userID string) error {
	channel := fmt.Sprintf("codespace:%s:user", codeSpaceID)
	data := map[string]interface{}{
		"type":      "user_joined",
		"userId":    userID,
		"timestamp": time.Now(),
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	return rm.client.Publish(rm.ctx, channel, jsonData).Err()
}

func (rm *RedisManager) PublishUserLeft(codeSpaceID, userID string) error {
	channel := fmt.Sprintf("codespace:%s:user", codeSpaceID)
	data := map[string]interface{}{
		"type":      "user_left",
		"userId":    userID,
		"timestamp": time.Now(),
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	return rm.client.Publish(rm.ctx, channel, jsonData).Err()
}

func (rm *RedisManager) PublishLanguageChange(codeSpaceID, language, userID string) error {
	channel := fmt.Sprintf("codespace:%s:language", codeSpaceID)
	data := map[string]interface{}{
		"type":      "language_changed",
		"language":  language,
		"userId":    userID,
		"timestamp": time.Now(),
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	return rm.client.Publish(rm.ctx, channel, jsonData).Err()
}

func (rm *RedisManager) CheckRateLimit(userID, action string, maxRequests int, windowSeconds int) (bool, error) {
	key := fmt.Sprintf("ratelimit:%s:%s", userID, action)

	count, err := rm.client.Get(rm.ctx, key).Int()
	if err == redis.Nil {
		// First request in window
		pipe := rm.client.Pipeline()
		pipe.Set(rm.ctx, key, 1, time.Duration(windowSeconds)*time.Second)
		_, err := pipe.Exec(rm.ctx)
		return err == nil, err
	}
	if err != nil {
		return false, err
	}

	if count >= maxRequests {
		return false, nil
	}

	// Increment counter
	return true, rm.client.Incr(rm.ctx, key).Err()
}

func (rm *RedisManager) Subscribe(ctx context.Context, handler func(channel string, message []byte)) {
	pubsub := rm.pubSubClient.PSubscribe(ctx, "codespace:*")
	defer pubsub.Close()

	ch := pubsub.Channel()

	for {
		select {
		case msg := <-ch:
			if msg != nil {
				handler(msg.Channel, []byte(msg.Payload))
			}
		case <-ctx.Done():
			return
		}
	}
}

func (rm *RedisManager) SetUserSession(userID string, session map[string]interface{}) error {
	key := fmt.Sprintf("user:%s:session", userID)
	jsonData, err := json.Marshal(session)
	if err != nil {
		return err
	}
	return rm.client.Set(rm.ctx, key, jsonData, 1*time.Hour).Err()
}

func (rm *RedisManager) GetUserSession(userID string) (map[string]interface{}, error) {
	key := fmt.Sprintf("user:%s:session", userID)
	data, err := rm.client.Get(rm.ctx, key).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var session map[string]interface{}
	if err := json.Unmarshal([]byte(data), &session); err != nil {
		return nil, err
	}

	return session, nil
}

func (rm *RedisManager) DeleteUserSession(userID string) error {
	key := fmt.Sprintf("user:%s:session", userID)
	return rm.client.Del(rm.ctx, key).Err()
}

func (rm *RedisManager) Close() error {
	if err := rm.client.Close(); err != nil {
		return err
	}
	return rm.pubSubClient.Close()
}
