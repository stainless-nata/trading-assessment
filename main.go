package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type SolanaRPCRequest struct {
	JSONRpc string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type SolanaRPCResponse struct {
	JSONRpc string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  struct {
		Context struct {
			Slot int `json:"slot"`
		} `json:"context"`
		Value int64 `json:"value"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type BalanceRequest struct {
	Wallets []string `json:"wallets"`
}

type WalletBalance struct {
	Address string  `json:"address"`
	Balance float64 `json:"balance"`
	Error   string  `json:"error,omitempty"`
}

type BalanceResponse struct {
	Success bool            `json:"success"`
	Data    []WalletBalance `json:"data"`
	Message string          `json:"message,omitempty"`
}

type CacheEntry struct {
	Balance   float64
	Timestamp time.Time
}

type RateLimiterEntry struct {
	Count     int
	ResetTime time.Time
}

type APIKey struct {
	Key       string    `bson:"key"`
	Active    bool      `bson:"active"`
	CreatedAt time.Time `bson:"created_at"`
}

var (
	solanaRPCURL   = getEnv("SOLANA_RPC_URL", "https://pomaded-lithotomies-xfbhnqagbt-dedicated.helius-rpc.com/?api-key=37ba4475-8fa3-4491-875f-758894981943")
	serverPort     = getEnv("SERVER_PORT", "8080")
	discordWebhook = getEnv("DISCORD_WEBHOOK", "https://discord.com/api/webhooks/your-webhook-url")
	mongoURI       = getEnv("MONGO_URI", "mongodb://localhost:27017")

	cache         = make(map[string]CacheEntry)
	cacheMutex    = sync.RWMutex{}
	requestMutex  = make(map[string]*sync.Mutex)
	mutexMapMutex = sync.RWMutex{}
	rateLimiters  = make(map[string]RateLimiterEntry)
	rateMutex     = sync.RWMutex{}

	mongoClient       *mongo.Client
	apiKeysCollection *mongo.Collection

	httpClient = &http.Client{Timeout: 10 * time.Second}
)

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func initMongoDB() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		return fmt.Errorf("failed to connect to MongoDB: %v", err)
	}

	if err := client.Ping(ctx, nil); err != nil {
		return fmt.Errorf("failed to ping MongoDB: %v", err)
	}

	mongoClient = client
	apiKeysCollection = client.Database("trading_api").Collection("api_keys")

	log.Println("✅ Connected to MongoDB")
	return nil
}

func validateAPIKey(apiKey string) bool {
	if apiKeysCollection != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var keyDoc APIKey
		err := apiKeysCollection.FindOne(ctx, bson.M{
			"key":    apiKey,
			"active": true,
		}).Decode(&keyDoc)

		return err == nil
	}

	log.Printf("API key validation failed - database unavailable and no fallback configured")
	return false
}

func isRateLimited(ip string) bool {
	rateMutex.Lock()
	defer rateMutex.Unlock()

	now := time.Now()
	entry, exists := rateLimiters[ip]

	if !exists || now.After(entry.ResetTime) {
		rateLimiters[ip] = RateLimiterEntry{
			Count:     1,
			ResetTime: now.Add(time.Minute),
		}
		return false
	}

	if entry.Count >= 10 {
		return true
	}

	entry.Count++
	rateLimiters[ip] = entry
	return false
}

func getWalletMutex(wallet string) *sync.Mutex {
	mutexMapMutex.RLock()
	mutex, exists := requestMutex[wallet]
	mutexMapMutex.RUnlock()

	if !exists {
		mutexMapMutex.Lock()
		mutex = &sync.Mutex{}
		requestMutex[wallet] = mutex
		mutexMapMutex.Unlock()
	}

	return mutex
}

func getFromCache(wallet string) (float64, bool) {
	cacheMutex.RLock()
	defer cacheMutex.RUnlock()

	entry, exists := cache[wallet]
	if !exists {
		return 0, false
	}

	if time.Since(entry.Timestamp) > 10*time.Second {
		return 0, false
	}

	return entry.Balance, true
}

func setInCache(wallet string, balance float64) {
	cacheMutex.Lock()
	defer cacheMutex.Unlock()

	cache[wallet] = CacheEntry{
		Balance:   balance,
		Timestamp: time.Now(),
	}
}

func fetchSolanaBalance(wallet string) (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	request := SolanaRPCRequest{
		JSONRpc: "2.0",
		ID:      1,
		Method:  "getBalance",
		Params:  []interface{}{wallet},
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal request: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", solanaRPCURL, bytes.NewBuffer(requestBody))
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return 0, fmt.Errorf("RPC request timeout after 8 seconds")
		}
		return 0, fmt.Errorf("failed to make request: %v", err)
	}
	defer resp.Body.Close()

	var rpcResponse SolanaRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResponse); err != nil {
		return 0, fmt.Errorf("failed to decode response: %v", err)
	}

	if rpcResponse.Error != nil {
		return 0, fmt.Errorf("RPC error: %s", rpcResponse.Error.Message)
	}

	balance := float64(rpcResponse.Result.Value) / 1000000000.0

	return balance, nil
}

func getWalletBalance(wallet string) (float64, error) {
	if balance, found := getFromCache(wallet); found {
		return balance, nil
	}

	mutex := getWalletMutex(wallet)
	mutex.Lock()
	defer mutex.Unlock()

	if balance, found := getFromCache(wallet); found {
		return balance, nil
	}

	balance, err := fetchSolanaBalance(wallet)
	if err != nil {
		return 0, err
	}

	setInCache(wallet, balance)
	return balance, nil
}

func sendJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(data)
}

func healthCheck(w http.ResponseWriter, r *http.Request) {
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"message":   "Solana Balance API is running",
		"timestamp": time.Now().Unix(),
	})
}

func getBalance(w http.ResponseWriter, r *http.Request) {
	apiKey := r.Header.Get("X-API-Key")
	if apiKey == "" {
		sendJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"success": false,
			"message": "API key required",
		})
		return
	}

	if !validateAPIKey(apiKey) {
		sendJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"success": false,
			"message": "Invalid API key",
		})
		return
	}

	clientIP := r.RemoteAddr
	if isRateLimited(clientIP) {
		sendJSON(w, http.StatusTooManyRequests, map[string]interface{}{
			"success": false,
			"message": "Rate limit exceeded. Maximum 10 requests per minute.",
		})
		return
	}

	var req BalanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "Invalid request format",
		})
		return
	}

	if len(req.Wallets) == 0 {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "At least one wallet address is required",
		})
		return
	}

	if len(req.Wallets) > 100 {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "Maximum 100 wallets per request",
		})
		return
	}

	type result struct {
		wallet  string
		balance float64
		err     error
	}

	results := make(chan result, len(req.Wallets))

	for _, wallet := range req.Wallets {
		go func(w string) {
			balance, err := getWalletBalance(w)
			results <- result{wallet: w, balance: balance, err: err}
		}(wallet)
	}

	balances := make([]WalletBalance, 0, len(req.Wallets))
	for i := 0; i < len(req.Wallets); i++ {
		result := <-results

		walletBalance := WalletBalance{
			Address: result.wallet,
			Balance: result.balance,
		}

		if result.err != nil {
			walletBalance.Error = result.err.Error()
		}

		balances = append(balances, walletBalance)
	}

	sendJSON(w, http.StatusOK, BalanceResponse{
		Success: true,
		Data:    balances,
	})
}

func panicRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				stack := make([]byte, 4096)
				length := runtime.Stack(stack, true)
				stack = stack[:length]

				log.Printf("Panic recovered: %v\nStack trace:\n%s", recovered, string(stack))

				go func() {
					payload := map[string]interface{}{
						"content": fmt.Sprintf("🚨 **PANIC DETECTED** 🚨\n```\nPanic: %v\nURL: %s %s\nIP: %s\nTime: %s\n\nFull Stack Trace:\n%s\n```",
							recovered,
							r.Method,
							r.URL.Path,
							r.RemoteAddr,
							time.Now().Format(time.RFC3339),
							string(stack),
						),
					}

					jsonPayload, _ := json.Marshal(payload)
					http.Post(discordWebhook, "application/json", bytes.NewBuffer(jsonPayload))
				}()

				sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
					"success": false,
					"message": "Internal server error",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func main() {
	if err := initMongoDB(); err != nil {
		log.Printf("⚠️ MongoDB initialization failed: %v", err)
		log.Println("⚠️ Continuing with fallback authentication...")
	}

	mux := http.NewServeMux()

	handler := panicRecovery(mux)

	mux.HandleFunc("/health", healthCheck)

	mux.HandleFunc("/api/get-balance", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
				"success": false,
				"message": "Method not allowed",
			})
			return
		}
		getBalance(w, r)
	})

	log.Printf("Starting Solana Balance API server on port %s", serverPort)

	if err := http.ListenAndServe(":"+serverPort, handler); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
