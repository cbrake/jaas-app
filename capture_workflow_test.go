package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func workflowParticipant() *ParticipantSession {
	return &ParticipantSession{RoomSlug: "sala-captura", DisplayName: "Ana", Email: "ana@cliente.example"}
}

func seedWorkflow(t *testing.T, e *testEnv, id string, data []byte, h CaptureHints) {
	t.Helper()
	if err := e.app.AppendChunkWithMetadata(workflowParticipant(), id, 0, e.now, data, h); err != nil {
		t.Fatal(err)
	}
}

func approveWorkflow(t *testing.T, e *testEnv, id int64) {
	t.Helper()
	finalizeAllCaptureStreams(t, e, id)
	e.advance(streamIdleClose + time.Second)
	if err := e.app.DB.ApproveCapture(id); err != nil {
		t.Fatal(err)
	}
}

// Fixture helper only: a real browser sends its own immutable totals after
// all upload acknowledgements. Querying server totals is not a client protocol.
func finalizeAllCaptureStreams(t *testing.T, e *testEnv, runID int64) {
	t.Helper()
	rows, err := e.app.DB.conn.Query("SELECT id,room_slug,email,display_name,next_seq,bytes FROM audio_streams WHERE run_id=? AND finalized_at IS NULL", runID)
	if err != nil {
		t.Fatal(err)
	}
	type stream struct {
		id   string
		p    ParticipantSession
		seq  int
		size int64
	}
	var streams []stream
	for rows.Next() {
		var s stream
		if err = rows.Scan(&s.id, &s.p.RoomSlug, &s.p.Email, &s.p.DisplayName, &s.seq, &s.size); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		streams = append(streams, s)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	for _, s := range streams {
		if err = e.app.FinalizeStream(&s.p, s.id, s.seq, s.size, e.now); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCaptureWaitsForExplicitApproval(t *testing.T) {
	e, ft := captureEnv(t)
	seedWorkflow(t, e, streamA, []byte("VOICE"), CaptureHints{})
	e.advance(20 * time.Minute)
	drain(e)
	if ft.calls != 0 {
		t.Fatalf("pending capture called provider %d times", ft.calls)
	}
	r, err := e.app.DB.GetCaptureRun(1)
	if err != nil {
		t.Fatal(err)
	}
	if !r.ApprovedAt.IsZero() || r.Status != "open" {
		t.Fatalf("pending state: %+v", r)
	}
	if err = e.app.DB.CancelCapture(1); err != nil {
		t.Fatal(err)
	}
	drain(e)
	if ft.calls != 0 {
		t.Fatal("cancelled capture called provider")
	}
	if _, err = os.Stat(e.app.streamPath(streamA)); err != nil {
		t.Fatal("cancel removed original audio")
	}
	if err = e.app.DB.ApproveCapture(1); !errors.Is(err, ErrCaptureCancelled) {
		t.Fatalf("reapproval: %v", err)
	}
}

func TestCaptureApprovalRejectsActiveAndStopsOldStreamWrites(t *testing.T) {
	e, _ := captureEnv(t)
	seedWorkflow(t, e, streamA, []byte("VOICE"), CaptureHints{})
	if err := e.app.DB.ApproveCapture(1); !errors.Is(err, ErrCaptureActive) {
		t.Fatalf("active approval: %v", err)
	}
	approveWorkflow(t, e, 1)
	err := e.app.AppendChunk(workflowParticipant(), streamA, 1, e.now, []byte("late"))
	if !errors.Is(err, errChunkClosed) {
		t.Fatalf("approved capture accepted a new chunk: %v", err)
	}
	if err = e.app.DB.ApproveCapture(1); err != nil {
		t.Fatalf("idempotent approve: %v", err)
	}
}

func TestCaptureChunkHashAndCrashTailRecovery(t *testing.T) {
	e, _ := captureEnv(t)
	p := workflowParticipant()
	seedWorkflow(t, e, streamA, []byte("first|"), CaptureHints{})
	if err := e.app.AppendChunk(p, streamA, 0, e.now, []byte("other|")); !errors.Is(err, errChunkConflict) {
		t.Fatalf("conflicting duplicate: %v", err)
	}
	_, err := e.app.DB.conn.Exec(`CREATE TRIGGER fail_chunk BEFORE INSERT ON audio_chunks WHEN NEW.seq=1 BEGIN SELECT RAISE(FAIL,'fixture commit failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.app.AppendChunk(p, streamA, 1, e.now, []byte("second|")); err == nil {
		t.Fatal("fixture did not fail")
	}
	if got := mustRead(t, e.app.streamPath(streamA)); got != "first|second|" {
		t.Fatalf("expected uncommitted tail: %q", got)
	}
	e.app.DB.conn.Exec("DROP TRIGGER fail_chunk")
	if err = e.app.AppendChunk(p, streamA, 1, e.now, []byte("second|")); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, e.app.streamPath(streamA)); got != "first|second|" {
		t.Fatalf("tail duplicated after retry: %q", got)
	}
	var n int
	e.app.DB.conn.QueryRow("SELECT COUNT(*) FROM audio_chunks WHERE stream_id=?", streamA).Scan(&n)
	if n != 2 {
		t.Fatalf("chunk ledger count %d", n)
	}
	// A shorter-than-committed source cannot silently be repaired by appending.
	if err = os.Truncate(e.app.streamPath(streamA), 3); err != nil {
		t.Fatal(err)
	}
	if err = e.app.AppendChunk(p, streamA, 2, e.now, []byte("third")); err == nil {
		t.Fatal("accepted corrupt committed prefix")
	}
}

func TestConcurrentDuplicateChunksWriteOnce(t *testing.T) {
	e, _ := captureEnv(t)
	var wg sync.WaitGroup
	errorsCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errorsCh <- e.app.AppendChunk(workflowParticipant(), streamA, 0, e.now, []byte("same"))
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := mustRead(t, e.app.streamPath(streamA)); got != "same" {
		t.Fatalf("duplicated concurrent data: %q", got)
	}
	var n int
	e.app.DB.conn.QueryRow("SELECT COUNT(*) FROM audio_chunks").Scan(&n)
	if n != 1 {
		t.Fatalf("ledger count %d", n)
	}
}

func TestASRResultSurvivesDatabaseFailureWithoutSecondRequest(t *testing.T) {
	e, ft := captureEnv(t)
	ft.byMark["VOICE"] = []transcriptSegment{{Text: "Se revisó el alcance del proyecto y la propuesta de capacitación.", TimePrecision: "window", RequestID: "req_fixture", Model: "gpt-transcribe", Usage: json.RawMessage(`{"seconds":15}`)}}
	seedWorkflow(t, e, streamA, []byte("VOICE"), CaptureHints{})
	approveWorkflow(t, e, 1)
	_, err := e.app.DB.conn.Exec(`CREATE TRIGGER fail_transcribed BEFORE UPDATE ON audio_streams WHEN NEW.status='transcribed' BEGIN SELECT RAISE(FAIL,'fixture persistence failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	e.app.transcribeStream(context.Background(), streamA, 1)
	if ft.calls != 1 {
		t.Fatalf("calls %d", ft.calls)
	}
	if _, err = os.Stat(e.app.resultPath(streamA)); err != nil {
		t.Fatal("durable result absent")
	}
	if _, err = os.Stat(e.app.streamPath(streamA)); err != nil {
		t.Fatal("original lost after DB failure")
	}
	e.app.DB.conn.Exec("DROP TRIGGER fail_transcribed")
	e.advance(16 * time.Minute)
	e.app.transcribeStream(context.Background(), streamA, 2)
	if ft.calls != 1 {
		t.Fatalf("recovery repeated provider request: %d", ft.calls)
	}
	var status string
	e.app.DB.conn.QueryRow("SELECT status FROM audio_streams WHERE id=?", streamA).Scan(&status)
	if status != "transcribed" {
		t.Fatalf("recovery status %s", status)
	}
	var n int
	e.app.DB.conn.QueryRow("SELECT COUNT(*) FROM ai_usage").Scan(&n)
	if n != 1 {
		t.Fatalf("cached recovery created fake request: %d", n)
	}
	var cost any
	e.app.DB.conn.QueryRow("SELECT estimated_cost FROM ai_usage").Scan(&cost)
	if cost != nil {
		t.Fatalf("unknown cost is not NULL: %v", cost)
	}
}

func TestUncertainASRRequestFailsClosed(t *testing.T) {
	e, ft := captureEnv(t)
	seedWorkflow(t, e, streamA, []byte("VOICE"), CaptureHints{})
	approveWorkflow(t, e, 1)
	if err := os.WriteFile(e.app.requestPath(streamA), []byte("previous request"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.app.transcribeStream(context.Background(), streamA, 2)
	if ft.calls != 0 {
		t.Fatal("retried an ambiguous prior provider call")
	}
	var status, msg string
	e.app.DB.conn.QueryRow("SELECT status,error FROM audio_streams WHERE id=?", streamA).Scan(&status, &msg)
	if status != "failed" || !strings.Contains(msg, "revisión manual") {
		t.Fatalf("state %s %q", status, msg)
	}
}

type cancellingCaptureTranscriber struct{ started chan struct{} }

func (t *cancellingCaptureTranscriber) Transcribe(ctx context.Context, _ []byte, _ string) ([]transcriptSegment, error) {
	close(t.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCancelInflightCaptureStopsNextStages(t *testing.T) {
	e, _ := captureEnv(t)
	transcriber := &cancellingCaptureTranscriber{started: make(chan struct{})}
	e.app.Transcriber = transcriber
	seedWorkflow(t, e, streamA, []byte("VOICE"), CaptureHints{})
	approveWorkflow(t, e, 1)
	done := make(chan struct{})
	go func() { defer close(done); e.app.transcribeStream(context.Background(), streamA, 1) }()
	select {
	case <-transcriber.started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider call never began")
	}
	if err := e.app.DB.CancelCapture(1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not propagate")
	}
	drain(e)
	var n int
	e.app.DB.conn.QueryRow("SELECT COUNT(*) FROM minutes").Scan(&n)
	if n != 0 {
		t.Fatal("cancelled capture generated minutes")
	}
	if _, err := os.Stat(e.app.streamPath(streamA)); err != nil {
		t.Fatal("cancel purged audio")
	}
}

func TestIncompleteCaptureNeverEnqueuesCompleteMinutes(t *testing.T) {
	e, _ := captureEnv(t)
	seedWorkflow(t, e, streamA, []byte("first"), CaptureHints{})
	seedWorkflow(t, e, streamB, []byte("second"), CaptureHints{})
	approveWorkflow(t, e, 1)
	e.app.DB.conn.Exec("UPDATE audio_streams SET status='transcribed',transcript=? WHERE id=?", `[{"text":"Una pista fue transcrita correctamente."}]`, streamA)
	e.app.DB.conn.Exec("UPDATE audio_streams SET status='failed',error='fixture' WHERE id=?", streamB)
	r, _ := e.app.DB.GetCaptureRun(1)
	e.app.finishRun(r)
	r, _ = e.app.DB.GetCaptureRun(1)
	if r.Status != "failed" || !strings.Contains(r.Error, "parcial") {
		t.Fatalf("run %+v", r)
	}
	var n int
	e.app.DB.conn.QueryRow("SELECT COUNT(*) FROM minutes").Scan(&n)
	if n != 0 {
		t.Fatal("partial capture produced complete minutes")
	}
}

func TestWindowAndSharedSpeakerRemainExplicitlyUnverified(t *testing.T) {
	e, ft := captureEnv(t)
	start := e.now
	ft.byMark["VOICE"] = []transcriptSegment{{Text: "Se discutieron el inventario, la capacitación y los siguientes pasos del proyecto.", TimePrecision: "window"}}
	seedWorkflow(t, e, streamA, []byte("VOICE"), CaptureHints{Language: "mixed", SharedMicrophone: true, MetadataJSON: `{"sample_rate":48000,"signal_test":"signal_detected","unknown_secret":"must not persist"}`})
	approveWorkflow(t, e, 1)
	e.app.transcribeStream(context.Background(), streamA, 1)
	r, _ := e.app.DB.GetCaptureRun(1)
	e.app.finishRun(r)
	tr, err := e.app.DB.GetTranscriptionBySession("sala-captura", "captura-1")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal([]byte(tr.Data), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["time_precision"] != "window" || doc["shared_microphone"] != true {
		t.Fatalf("doc %s", tr.Data)
	}
	message := doc["messages"].([]any)[0].(map[string]any)
	if message["speaker_verified"] != false || message["duration_ms"] != nil {
		t.Fatalf("unverified speech presented as measured: %v", message)
	}
	if !strings.Contains(message["name"].(string), "sin verificar") {
		t.Fatal("shared microphone assigned to account holder")
	}
	ms, _ := e.app.DB.ListMinutes("sala-captura")
	if len(ms) != 1 || !ms[0].MeetingAt.Equal(start) {
		t.Fatalf("meeting start changed: %+v", ms)
	}
	var metadata string
	e.app.DB.conn.QueryRow("SELECT metadata_json FROM audio_streams WHERE id=?", streamA).Scan(&metadata)
	if strings.Contains(metadata, "must not persist") {
		t.Fatal("unknown metadata persisted")
	}
}

func TestExpiredPendingAudioCannotResumeAndFreshActiveIsPreserved(t *testing.T) {
	e, _ := captureEnv(t)
	seedWorkflow(t, e, streamA, []byte("pending"), CaptureHints{})
	old := e.now.AddDate(0, 0, -31)
	os.Chtimes(e.app.streamPath(streamA), old, old)
	if n := e.app.purgeOrphanAudio(); n != 0 {
		t.Fatalf("deleted currently active source: %d", n)
	}
	e.advance(streamIdleClose + time.Second)
	if n := e.app.purgeOrphanAudio(); n != 1 {
		t.Fatalf("expired audio count %d", n)
	}
	r, _ := e.app.DB.GetCaptureRun(1)
	if r.Status != "failed" || !strings.Contains(r.Error, "retención") {
		t.Fatalf("expired pending silently resumable %+v", r)
	}
	if err := e.app.DB.ApproveCapture(1); err == nil {
		t.Fatal("approved expired audio")
	}
}

func TestCaptureQuotaAndUploadMetadata(t *testing.T) {
	e, _ := captureEnv(t)
	e.app.Config.AudioMaxStorageBytes = 4
	if err := e.app.AppendChunk(workflowParticipant(), streamA, 0, e.now, []byte("12345")); !errors.Is(err, errAudioQuota) {
		t.Fatalf("quota %v", err)
	}
	if err := e.app.AppendChunk(workflowParticipant(), "../../escape", 0, e.now, []byte("x")); !errors.Is(err, errChunkForbidden) {
		t.Fatalf("path %v", err)
	}
	if _, err := normalizeCaptureHints(CaptureHints{Language: "es-MX"}); err == nil {
		t.Fatal("accepted unsupported API language")
	}
	if _, err := normalizeCaptureHints(CaptureHints{MetadataJSON: `{"signal_test":"invented"}`}); err == nil {
		t.Fatal("accepted invalid metadata enum")
	}
	ana := joinCaptureRoom(t, e, "sala-captura", "Ana", "ana@cliente.example", false)
	if status := ana.upload(e, "sala-captura", streamA, 0, []byte("12345"), ana.csrf); status != http.StatusInsufficientStorage {
		t.Fatalf("quota HTTP %d", status)
	}
}

func TestCorruptAudioIsRejectedBeforeProvider(t *testing.T) {
	e, ft := captureEnv(t)
	seedWorkflow(t, e, streamA, []byte("VOICE"), CaptureHints{})
	approveWorkflow(t, e, 1)
	if err := os.WriteFile(e.app.streamPath(streamA), []byte("OTHER"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.app.transcribeStream(context.Background(), streamA, 1)
	if ft.calls != 0 {
		t.Fatal("corrupt source was sent to provider")
	}
	var status, usageStatus string
	e.app.DB.conn.QueryRow("SELECT status FROM audio_streams WHERE id=?", streamA).Scan(&status)
	e.app.DB.conn.QueryRow("SELECT status FROM ai_usage WHERE capture_id=1").Scan(&usageStatus)
	if status != "failed" || usageStatus != "not_sent" {
		t.Fatalf("state %s / usage %s", status, usageStatus)
	}
}

func TestWindowTranscriptDoesNotUseLegacyPhraseBlacklist(t *testing.T) {
	for _, text := range []string{"Gracias.", "Sí.", "Ajá.", "Gracias por su atención"} {
		if !keepSegment(transcriptSegment{Text: text, TimePrecision: "window"}) {
			t.Fatalf("source speech silently removed: %q", text)
		}
	}
	if keepSegment(transcriptSegment{Text: "  ", TimePrecision: "window"}) {
		t.Fatal("empty source accepted")
	}
}

func TestMissingTerminalManifestCannotBeApproved(t *testing.T) {
	e, ft := captureEnv(t)
	seedWorkflow(t, e, streamA, []byte("first|"), CaptureHints{})
	e.advance(10 * time.Minute)
	e.app.processCapture(context.Background())
	if err := e.app.DB.ApproveCapture(1); !errors.Is(err, ErrCaptureIncomplete) {
		t.Fatalf("idle partial approved: %v", err)
	}
	// The browser reports a second final chunk which never arrived.
	if err := e.app.FinalizeStream(workflowParticipant(), streamA, 2, 12, e.now); !errors.Is(err, ErrCaptureIncomplete) {
		t.Fatalf("missing final chunk accepted: %v", err)
	}
	drain(e)
	if ft.calls != 0 {
		t.Fatal("partial capture consumed provider")
	}
	var finalized any
	e.app.DB.conn.QueryRow("SELECT finalized_at FROM audio_streams WHERE id=?", streamA).Scan(&finalized)
	if finalized != nil {
		t.Fatal("invalid manifest marked finalized")
	}
}

func TestFinalManifestIsOwnedIdempotentAndPreventsFurtherChunks(t *testing.T) {
	e, _ := captureEnv(t)
	seedWorkflow(t, e, streamA, []byte("VOICE"), CaptureHints{})
	foreign := workflowParticipant()
	foreign.Email = "other@cliente.example"
	if err := e.app.FinalizeStream(foreign, streamA, 1, 5, e.now); !errors.Is(err, errChunkForbidden) {
		t.Fatalf("foreign finalization: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := e.app.FinalizeStream(workflowParticipant(), streamA, 1, 5, e.now); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.app.FinalizeStream(workflowParticipant(), streamA, 2, 10, e.now); !errors.Is(err, ErrCaptureIncomplete) {
		t.Fatalf("changed terminal manifest: %v", err)
	}
	if err := e.app.AppendChunk(workflowParticipant(), streamA, 1, e.now, []byte("late")); !errors.Is(err, errChunkClosed) {
		t.Fatalf("append after final: %v", err)
	}
	if err := e.app.DB.ApproveCapture(1); err != nil {
		t.Fatalf("explicit finalization should remove idle wait: %v", err)
	}
}
