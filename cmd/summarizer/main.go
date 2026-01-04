package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	agentv1 "github.com/coretexos/cap/v2/coretex/agent/v1"
	"github.com/coretexos/cap/v2/sdk/go/worker"
	"github.com/coretexos/coretex-incident-enricher/internal/artifacts"
	"github.com/coretexos/coretex-incident-enricher/internal/config"
	"github.com/coretexos/coretex-incident-enricher/internal/gatewayclient"
	"github.com/coretexos/coretex-incident-enricher/internal/llm"
	"github.com/coretexos/coretex-incident-enricher/internal/policyconstraints"
	"github.com/coretexos/coretex-incident-enricher/internal/store"
	"github.com/coretexos/coretex-incident-enricher/internal/types"
	"github.com/nats-io/nats.go"
)

type summarizerInput struct {
	Evidence types.EvidenceBundle `json:"evidence"`
}

func main() {
	cfg := config.Load("summarizer")

	nc, err := nats.Connect(cfg.NATSURL)
	if err != nil {
		log.Fatal(err)
	}
	defer nc.Close()

	mem, err := store.New(cfg.RedisURL, cfg.DataTTL)
	if err != nil {
		log.Fatal(err)
	}

	gw := gatewayclient.New(cfg.GatewayURL, cfg.APIKey)

	handler := func(ctx context.Context, req *agentv1.JobRequest) (*agentv1.JobResult, error) {
		start := time.Now()
		ctxPtr := req.GetContextPtr()
		if ctxPtr == "" && req.Env != nil {
			ctxPtr = req.Env["context_ptr"]
		}
		var input summarizerInput
		if err := mem.GetContextJSON(ctx, ctxPtr, &input); err != nil {
			return nil, err
		}
		if input.Evidence.IncidentID == "" {
			return nil, errors.New("missing evidence in input")
		}
		redaction := policyconstraints.RedactionLevel(req.Env)
		summary, err := llm.Summarize(ctx, cfg.LLMProvider, input.Evidence, redaction)
		if err != nil {
			return nil, err
		}
		maxBytes := policyconstraints.MaxArtifactBytes(req.Env)
		ptr, _, err := artifacts.UploadText(ctx, gw, summary.SummaryMarkdown, "text/markdown", "audit", map[string]string{
			"kind":       "summary",
			"incident_id": summary.IncidentID,
		}, maxBytes)
		if err != nil {
			return nil, err
		}
		summary.ArtifactPtr = ptr

		resultPtr, err := mem.PutResultJSON(ctx, req.GetJobId(), summary)
		if err != nil {
			return nil, err
		}
		return &agentv1.JobResult{
			JobId:        req.GetJobId(),
			Status:       agentv1.JobStatus_JOB_STATUS_SUCCEEDED,
			ResultPtr:    resultPtr,
			WorkerId:     cfg.WorkerID,
			ExecutionMs:  time.Since(start).Milliseconds(),
			ArtifactPtrs: []string{ptr},
		}, nil
	}

	subject := fmt.Sprintf("worker.%s.jobs", cfg.WorkerID)
	w := &worker.Worker{
		NATS:     nc,
		Subject:  subject,
		Handler:  handler,
		SenderID: cfg.WorkerID,
	}
	if err := w.Start(); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go worker.HeartbeatLoop(ctx, nc, func() ([]byte, error) {
		return worker.HeartbeatPayload(cfg.WorkerID, cfg.WorkerPool, 0, cfg.MaxParallelJobs, 0)
	})

	log.Printf("summarizer listening on %s for job.incident-enricher.summarize (worker_id=%s pool=%s)", subject, cfg.WorkerID, cfg.WorkerPool)
	<-ctx.Done()
}
