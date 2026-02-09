package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

const (
	dvmKindTextGen      = 5050
	dvmKindTextResult   = 6050
	dvmKindStatus       = 7000
	dvmKindAnnouncement = 31990
	defaultPriceSats    = 10
	maxPromptLen        = 2000
	invoicePollInterval = 5 * time.Second
	invoicePollTimeout  = 10 * time.Minute
	groqModel           = "llama-3.3-70b-versatile"
	freeQueriesPerUser  = 3
)

var relays = []string{
	"wss://relay.damus.io",
	"wss://nos.lol",
	"wss://relay.primal.net",
}

type Config struct {
	NostrNsec  string
	LNbitsURL  string
	LNbitsKey  string
	LNbitsAuth string // basic auth user:pass
	GroqAPIKey string
}

// freeTracker tracks which pubkeys have used their free query.
// Persists to a file so it survives restarts.
type freeTracker struct {
	mu   sync.Mutex
	used map[string]int
	path string
}

func newFreeTracker(dir string) *freeTracker {
	ft := &freeTracker{
		used: make(map[string]int),
		path: filepath.Join(dir, "free-queries.txt"),
	}
	ft.load()
	return ft
}

func (ft *freeTracker) load() {
	f, err := os.Open(ft.path)
	if err != nil {
		return // file doesn't exist yet
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			ft.used[line]++
		}
	}
}

func (ft *freeTracker) hasFree(pubkey string) bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.used[pubkey] < freeQueriesPerUser
}

func (ft *freeTracker) useFree(pubkey string) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.used[pubkey]++
	// Append to file
	f, err := os.OpenFile(ft.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to persist free query: %v", err)
		return
	}
	defer f.Close()
	fmt.Fprintln(f, pubkey)
}

func loadConfig() Config {
	return Config{
		NostrNsec:  os.Getenv("NOSTR_NSEC"),
		LNbitsURL:  envOr("LNBITS_URL", "https://lnbits.klabo.world"),
		LNbitsKey:  os.Getenv("LNBITS_KEY"),
		LNbitsAuth: os.Getenv("LNBITS_AUTH"),
		GroqAPIKey: os.Getenv("GROQ_API_KEY"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := loadConfig()
	if cfg.NostrNsec == "" || cfg.LNbitsKey == "" || cfg.GroqAPIKey == "" {
		log.Fatal("Required env vars: NOSTR_NSEC, LNBITS_KEY, GROQ_API_KEY, LNBITS_AUTH")
	}

	var sk string
	if strings.HasPrefix(cfg.NostrNsec, "nsec") {
		_, v, err := nip19.Decode(cfg.NostrNsec)
		if err != nil {
			log.Fatalf("failed to decode nsec: %v", err)
		}
		sk = v.(string)
	} else {
		sk = cfg.NostrNsec
	}
	pub, _ := nostr.GetPublicKey(sk)
	log.Printf("DVM pubkey: %s", pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Shutting down...")
		cancel()
	}()

	pool := nostr.NewSimplePool(ctx)

	// Publish NIP-89 announcement
	publishAnnouncement(ctx, pool, sk, pub)

	var processed sync.Map
	freeDir := envOr("STATE_DIR", "/var/lib/dvm-textgen")
	os.MkdirAll(freeDir, 0755)
	freeTier := newFreeTracker(freeDir)
	log.Printf("Free tier: %d queries per user (state: %s)", freeQueriesPerUser, freeTier.path)

	// Subscribe to kind 5050 starting from now (no backfill)
	since := nostr.Timestamp(time.Now().Add(-60 * time.Second).Unix())
	filters := nostr.Filters{{
		Kinds: []int{dvmKindTextGen},
		Since: &since,
	}}

	log.Printf("Subscribing to kind %d on %v", dvmKindTextGen, relays)

	for ev := range pool.SubMany(ctx, relays, filters) {
		go handleRequest(ctx, ev.Event, sk, pub, cfg, &processed, pool, freeTier)
	}

	log.Println("Subscription ended")
}

func handleRequest(ctx context.Context, ev *nostr.Event, sk, pub string, cfg Config, processed *sync.Map, pool *nostr.SimplePool, freeTier *freeTracker) {
	if _, loaded := processed.LoadOrStore(ev.ID, true); loaded {
		return // Already processed — prevents triple-response bug
	}

	// Skip events older than 5 minutes
	if time.Since(time.Unix(int64(ev.CreatedAt), 0)) > 5*time.Minute {
		log.Printf("Skipping old event %s (age: %v)", ev.ID[:8], time.Since(time.Unix(int64(ev.CreatedAt), 0)))
		return
	}

	// Skip if targeted at another DVM
	for _, tag := range ev.Tags {
		if tag[0] == "p" && len(tag) > 1 && tag[1] != pub {
			log.Printf("Skipping event %s: targeted at %s", ev.ID[:8], tag[1][:8])
			return
		}
	}

	// Skip CI/automation requests
	for _, tag := range ev.Tags {
		if tag[0] == "cmd" || tag[0] == "secret" {
			log.Printf("Skipping CI/automation event %s", ev.ID[:8])
			return
		}
	}

	log.Printf("Processing request %s from %s", ev.ID[:8], ev.PubKey[:8])

	// Extract prompt from i tag
	prompt := ""
	for _, tag := range ev.Tags {
		if tag[0] == "i" && len(tag) > 1 {
			prompt = tag[1]
			break
		}
	}
	if prompt == "" {
		log.Printf("No prompt in event %s", ev.ID[:8])
		sendStatus(ctx, pool, sk, pub, ev, "error", "No prompt found. Use an 'i' tag with your text prompt.")
		return
	}
	if len(prompt) > maxPromptLen {
		prompt = prompt[:maxPromptLen]
	}

	// Determine price from bid tag
	priceSats := defaultPriceSats
	hasBid := false
	for _, tag := range ev.Tags {
		if tag[0] == "bid" && len(tag) > 1 {
			hasBid = true
			var bid int
			fmt.Sscanf(tag[1], "%d", &bid)
			if bid > 0 {
				priceSats = bid / 1000 // bid is in msats
				if priceSats < 1 {
					priceSats = 1
				}
			}
		}
	}

	// Free tier: first query free for each user (no bid tag = eligible for free)
	isFreeQuery := false
	if !hasBid && freeTier.hasFree(ev.PubKey) {
		isFreeQuery = true
		log.Printf("FREE TIER: first query for %s", ev.PubKey[:8])
		freeTier.useFree(ev.PubKey)
		sendStatus(ctx, pool, sk, pub, ev, "processing", "First query is free! Generating with Llama 3.3 70B...")
	} else {
		// Create LNbits invoice
		invoice, paymentHash, err := createInvoice(cfg, priceSats, fmt.Sprintf("DVM Text: %.50s", prompt))
		if err != nil {
			log.Printf("Invoice creation failed: %v", err)
			sendStatus(ctx, pool, sk, pub, ev, "error", "Failed to create Lightning invoice")
			return
		}

		log.Printf("Invoice created: %d sats, hash: %s", priceSats, paymentHash[:8])
		sendPaymentRequired(ctx, pool, sk, pub, ev, invoice, priceSats)

		paid := pollForPayment(ctx, cfg, paymentHash)
		if !paid {
			log.Printf("Payment timeout for %s", ev.ID[:8])
			return
		}

		log.Printf("PAID! %d sats for %s", priceSats, ev.ID[:8])
	}

	log.Printf("Generating text for %s (free=%v)...", ev.ID[:8], isFreeQuery)
	sendStatus(ctx, pool, sk, pub, ev, "processing", "Generating response with Llama 3.3 70B...")

	response, err := generateText(cfg, prompt)
	if err != nil {
		log.Printf("Text gen failed: %v", err)
		sendStatus(ctx, pool, sk, pub, ev, "error", fmt.Sprintf("Generation failed: %v", err))
		return
	}

	log.Printf("Text generated: %d chars for %s", len(response), ev.ID[:8])
	sendResult(ctx, pool, sk, pub, ev, response)
	log.Printf("DELIVERED result for %s — %d sats earned!", ev.ID[:8], priceSats)
}

func publishAnnouncement(ctx context.Context, pool *nostr.SimplePool, sk, pub string) {
	content := `{"name":"Maximum Sats AI Text Gen","about":"AI text generation via Llama 3.3 70B. First 3 queries FREE, then 10 sats via Lightning. Send kind 5050 with 'i' tag prompt.","picture":"https://maximumsats.com/favicon.ico","nip90Params":{}}`

	ev := nostr.Event{
		Kind:      dvmKindAnnouncement,
		PubKey:    pub,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "maximum-sats-textgen"},
			{"k", fmt.Sprintf("%d", dvmKindTextGen)},
			{"t", "text-generation"},
		},
		Content: content,
	}
	ev.Sign(sk)

	for _, url := range relays {
		relay, err := pool.EnsureRelay(url)
		if err != nil {
			log.Printf("Failed to connect to %s: %v", url, err)
			continue
		}
		if err := relay.Publish(ctx, ev); err != nil {
			log.Printf("Announcement failed on %s: %v", url, err)
		} else {
			log.Printf("Announced on %s", url)
		}
	}
}

func sendStatus(ctx context.Context, pool *nostr.SimplePool, sk, pub string, req *nostr.Event, status, msg string) {
	ev := nostr.Event{
		Kind:      dvmKindStatus,
		PubKey:    pub,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", req.ID},
			{"p", req.PubKey},
			{"status", status, msg},
		},
	}
	ev.Sign(sk)
	publishToRelays(ctx, pool, ev)
}

func sendPaymentRequired(ctx context.Context, pool *nostr.SimplePool, sk, pub string, req *nostr.Event, bolt11 string, amountSats int) {
	ev := nostr.Event{
		Kind:      dvmKindStatus,
		PubKey:    pub,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", req.ID},
			{"p", req.PubKey},
			{"status", "payment-required"},
			{"amount", fmt.Sprintf("%d", amountSats*1000), bolt11},
		},
	}
	ev.Sign(sk)
	publishToRelays(ctx, pool, ev)
}

func sendResult(ctx context.Context, pool *nostr.SimplePool, sk, pub string, req *nostr.Event, text string) {
	reqJSON, _ := json.Marshal(req)
	ev := nostr.Event{
		Kind:      dvmKindTextResult,
		PubKey:    pub,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", req.ID},
			{"p", req.PubKey},
			{"request", string(reqJSON)},
		},
		Content: text,
	}
	ev.Sign(sk)
	publishToRelays(ctx, pool, ev)
}

func publishToRelays(ctx context.Context, pool *nostr.SimplePool, ev nostr.Event) {
	for _, url := range relays {
		relay, err := pool.EnsureRelay(url)
		if err != nil {
			continue
		}
		relay.Publish(ctx, ev)
	}
}

func createInvoice(cfg Config, amountSats int, memo string) (bolt11 string, paymentHash string, err error) {
	body, _ := json.Marshal(map[string]interface{}{
		"out":    false,
		"amount": amountSats,
		"memo":   memo,
	})

	req, _ := http.NewRequest("POST", cfg.LNbitsURL+"/api/v1/payments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", cfg.LNbitsKey)
	if cfg.LNbitsAuth != "" {
		parts := strings.SplitN(cfg.LNbitsAuth, ":", 2)
		if len(parts) == 2 {
			req.SetBasicAuth(parts[0], parts[1])
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("lnbits request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("lnbits returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		PaymentRequest string `json:"payment_request"`
		PaymentHash    string `json:"payment_hash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("decode failed: %w", err)
	}
	return result.PaymentRequest, result.PaymentHash, nil
}

func pollForPayment(ctx context.Context, cfg Config, paymentHash string) bool {
	deadline := time.After(invoicePollTimeout)
	ticker := time.NewTicker(invoicePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline:
			return false
		case <-ticker.C:
			paid, err := checkPayment(cfg, paymentHash)
			if err != nil {
				log.Printf("Payment check error: %v", err)
				continue
			}
			if paid {
				return true
			}
		}
	}
}

func checkPayment(cfg Config, paymentHash string) (bool, error) {
	req, _ := http.NewRequest("GET", cfg.LNbitsURL+"/api/v1/payments/"+paymentHash, nil)
	req.Header.Set("X-Api-Key", cfg.LNbitsKey)
	if cfg.LNbitsAuth != "" {
		parts := strings.SplitN(cfg.LNbitsAuth, ":", 2)
		if len(parts) == 2 {
			req.SetBasicAuth(parts[0], parts[1])
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	var result struct {
		Paid bool `json:"paid"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Paid, nil
}

func generateText(cfg Config, prompt string) (string, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"model": groqModel,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a helpful AI assistant. Respond concisely and accurately."},
			{"role": "user", "content": prompt},
		},
		"max_tokens":  1024,
		"temperature": 0.7,
	})

	req, _ := http.NewRequest("POST", "https://api.groq.com/openai/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.GroqAPIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("groq request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("groq returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode groq response: %w", err)
	}
	if len(result.Choices) == 0 || result.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("groq returned no content")
	}

	return result.Choices[0].Message.Content, nil
}
