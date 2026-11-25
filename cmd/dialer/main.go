package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"smartdialer/internal/allocator"
	"smartdialer/internal/domain"
	"smartdialer/internal/pacing"
	"smartdialer/internal/provider"
	"smartdialer/internal/safety"
	"smartdialer/internal/store"
	"smartdialer/internal/worker"
)

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

var (
	simMu      sync.Mutex
	simRunning bool
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://postgres:postgres@localhost:5432/smartdialer?sslmode=disable"
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Initialize PostgreSQL Pool
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		logger.Error("failed to parse database configuration", "err", err)
		os.Exit(1)
	}
	poolConfig.MaxConns = 30
	poolConfig.MinConns = 5

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		logger.Error("failed to create database pool", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		logger.Error("database ping failed", "err", err)
		os.Exit(1)
	}
	logger.Info("connected to PostgreSQL", "database_url", databaseURL)

	st := store.NewPostgresStore(pool)
	if err := st.Migrate(ctx); err != nil {
		logger.Error("failed to execute migrations", "err", err)
		os.Exit(1)
	}
	logger.Info("database schema migrations applied successfully")

	// 2. Telecom Carrier Initialization
	provA := provider.NewMockProviderA(0.40, 90*time.Second)
	provA.RegisterWebhookHandler(func(ctx context.Context, event domain.CallEvent) error {
		return st.ProcessProviderEvent(ctx, event)
	})

	// 3. Pacing Engines & Safety Controller
	progressiveEngine := pacing.NewProgressiveEngine()
	predictiveEngine := pacing.NewPredictiveEngine()
	safetyController := safety.NewController(st)
	callAllocator := allocator.NewAllocator(st, provA)

	// 4. Worker Pool & Stale-State Reaper
	workerPool := worker.NewPool(4, st, progressiveEngine, safetyController, callAllocator, logger)
	reaper := worker.NewReaper(st, worker.DefaultReaperConfig(), logger)

	// Launch Reaper
	go reaper.Start(ctx)

	// Active campaign tracking
	var activeCampaignID = uuid.New()
	var currentMode = "PROGRESSIVE"

	// 5. HTTP Router
	mux := http.NewServeMux()

	// Healthcheck
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"database": "connected",
			"carrier":  provA.Health(),
			"time":     time.Now().UTC(),
		})
	})

	// --------------------------------------------------------------------------------------------
	// V1 API Endpoints for Frontend Dashboard
	// --------------------------------------------------------------------------------------------

	// GET /api/v1/metrics/live: Real-time agent state grid, active calls, answer rate, safety decisions
	mux.HandleFunc("GET /api/v1/metrics/live", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Query agent state breakdown
		var available, reserved, dialing, connected, wrapUp, paused, offline int
		_ = pool.QueryRow(r.Context(), `
			SELECT 
				COALESCE(COUNT(*) FILTER (WHERE state = 'AVAILABLE'), 0),
				COALESCE(COUNT(*) FILTER (WHERE state = 'RESERVED'), 0),
				COALESCE(COUNT(*) FILTER (WHERE state = 'DIALING'), 0),
				COALESCE(COUNT(*) FILTER (WHERE state = 'CONNECTED'), 0),
				COALESCE(COUNT(*) FILTER (WHERE state = 'WRAP_UP'), 0),
				COALESCE(COUNT(*) FILTER (WHERE state = 'PAUSED'), 0),
				COALESCE(COUNT(*) FILTER (WHERE state = 'OFFLINE'), 0)
			FROM agents;
		`).Scan(&available, &reserved, &dialing, &connected, &wrapUp, &paused, &offline)

		metrics, _ := st.GetPacingMetrics(r.Context(), activeCampaignID)
		stats := workerPool.Stats()

		// Circuit Breaker logic
		circuitBreaker := "CLOSED"
		if metrics != nil && metrics.ProviderErrorRate > 0.25 {
			circuitBreaker = "OPEN"
		} else if metrics != nil && metrics.RollingAbandonmentRate > 0.02 {
			circuitBreaker = "HALF-OPEN"
		}

		// Fetch recent safety audit decisions
		rows, _ := pool.Query(r.Context(), `
			SELECT id, mode, proposed_calls, approved_calls, decision, reason, timestamp
			FROM pacing_audit_logs
			ORDER BY timestamp DESC
			LIMIT 15;
		`)
		var recentDecisions []map[string]any
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var id uuid.UUID
				var mode, decision, reason string
				var prop, app int
				var ts time.Time
				if err := rows.Scan(&id, &mode, &prop, &app, &decision, &reason, &ts); err == nil {
					recentDecisions = append(recentDecisions, map[string]any{
						"id":             id,
						"mode":           mode,
						"proposed_calls": prop,
						"approved_calls": app,
						"decision":       decision,
						"reason":         reason,
						"timestamp":      ts,
					})
				}
			}
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"timestamp": time.Now().UTC(),
			"mode":      currentMode,
			"agent_states": map[string]int{
				"AVAILABLE": available,
				"RESERVED":  reserved,
				"DIALING":   dialing,
				"CONNECTED": connected,
				"WRAP_UP":   wrapUp,
				"PAUSED":    paused,
				"OFFLINE":   offline,
				"TOTAL":     available + reserved + dialing + connected + wrapUp + paused + offline,
			},
			"active_calls": map[string]int{
				"connected": metrics.ConnectedCalls,
				"ringing":   metrics.RingingCalls,
				"in_flight": metrics.InFlightCalls,
			},
			"telemetry": map[string]float64{
				"rolling_answer_rate":      metrics.RollingAnswerRate,
				"rolling_abandonment_rate": metrics.RollingAbandonmentRate,
				"provider_error_rate":      metrics.ProviderErrorRate,
				"agent_drop_rate_5s":       metrics.AgentDropRateLast5s,
			},
			"circuit_breaker":  circuitBreaker,
			"worker_stats":     stats,
			"recent_decisions": recentDecisions,
		})
	})

	// POST /api/v1/simulator/run: Triggers simulation execution
	mux.HandleFunc("POST /api/v1/simulator/run", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		simMu.Lock()
		if simRunning {
			simMu.Unlock()
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "simulation is already running"})
			return
		}
		simRunning = true
		simMu.Unlock()

		var req struct {
			Target string `json:"target"` // "all", "scenarios", "failures", "load"
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Target == "" {
			req.Target = "all"
		}

		go func(target string) {
			defer func() {
				simMu.Lock()
				simRunning = false
				simMu.Unlock()
			}()

			flagArg := "--" + target
			cmd := exec.Command("go", "run", "cmd/simulator/main.go", flagArg)
			cmd.Env = os.Environ()
			out, err := cmd.CombinedOutput()
			if err != nil {
				slog.Error("simulation run failed", "err", err, "out", string(out))
			} else {
				slog.Info("simulation run completed successfully", "target", target)
			}
		}(req.Target)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "started",
			"target":  req.Target,
			"message": "simulation started in background",
		})
	})

	// GET /api/v1/simulator/results: Returns latest simulation results JSON
	mux.HandleFunc("GET /api/v1/simulator/results", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		data, err := os.ReadFile("simulation_results.json")
		if err != nil {
			// Try web public folder
			data, err = os.ReadFile("web/public/simulation_results.json")
			if err != nil {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "simulation_results.json not found yet; please run simulator first"})
				return
			}
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})

	// Agent & Borrower Management
	mux.HandleFunc("POST /api/agents", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name  string            `json:"name"`
			State domain.AgentState `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.State == "" {
			req.State = domain.AgentAvailable
		}
		agent, err := st.CreateAgent(r.Context(), req.Name, req.State)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(agent)
	})

	mux.HandleFunc("POST /api/borrowers", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PhoneNumber string `json:"phone_number"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		borrower, err := st.CreateBorrower(r.Context(), req.PhoneNumber)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(borrower)
	})

	mux.HandleFunc("POST /api/campaigns/start", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			CampaignID string `json:"campaign_id"`
			Mode       string `json:"mode"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		campUUID := uuid.New()
		if req.CampaignID != "" {
			if parsed, err := uuid.Parse(req.CampaignID); err == nil {
				campUUID = parsed
			}
		}
		activeCampaignID = campUUID

		if req.Mode == "PREDICTIVE" {
			currentMode = "PREDICTIVE"
			workerPool.SetEngine(predictiveEngine)
		} else {
			currentMode = "PROGRESSIVE"
			workerPool.SetEngine(progressiveEngine)
		}

		workerPool.Start(ctx, campUUID)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message":     "campaign dialer started",
			"campaign_id": campUUID,
			"mode":        currentMode,
		})
	})

	mux.HandleFunc("POST /api/campaigns/stop", func(w http.ResponseWriter, r *http.Request) {
		workerPool.Stop()
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "campaign dialer stopped"})
	})

	mux.HandleFunc("POST /api/webhooks/{provider}", func(w http.ResponseWriter, r *http.Request) {
		var event domain.CallEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if event.ID == uuid.Nil {
			event.ID = uuid.New()
		}
		if event.EventTimestamp.IsZero() {
			event.EventTimestamp = time.Now().UTC()
		}

		if err := st.ProcessProviderEvent(r.Context(), event); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "processed"})
	})

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      withCORS(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// 6. Graceful Shutdown Management
	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		logger.Info("SmartDialer server listening", "port", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server listener error", "err", err)
			os.Exit(1)
		}
	}()

	sig := <-shutdownCh
	logger.Info("received shutdown signal, beginning graceful shutdown", "signal", sig.String())

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	workerPool.Stop()
	cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", "err", err)
	}

	logger.Info("SmartDialer process exited cleanly")
}
