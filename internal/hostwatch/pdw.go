package hostwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// PDW sends notices through the personal data warehouse's notify tool (POST /api/tools/notify), the
// push channel to the account holder's phone. Key is a PDW access token whose scope reaches the
// tools API (an agent token).
type PDW struct {
	URL, Key string
	Client   *http.Client
}

type notifyInput struct {
	Title             string `json:"title"`
	Body              string `json:"body,omitempty"`
	ThreadID          string `json:"thread_id,omitempty"`
	InterruptionLevel string `json:"interruption_level,omitempty"`
}

// Notify sends n; it fails unless PDW says it delivered to at least one device without error.
func (p *PDW) Notify(ctx context.Context, n Notice) error {
	body, err := json.Marshal(notifyInput{Title: n.Title, Body: n.Body, ThreadID: n.ThreadID, InterruptionLevel: n.Level})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.URL, "/")+"/api/tools/notify", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer intelligence:"+p.Key)
	req.Header.Set("Content-Type", "application/json")
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("PDW notify request failed")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PDW notify HTTP %d: %s", resp.StatusCode, Clip(strings.TrimSpace(string(raw)), 200))
	}
	var out struct {
		Data struct {
			Error  string `json:"error"`
			Report struct {
				Devices int `json:"devices"`
				Sent    int `json:"sent"`
			} `json:"report"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decode PDW notify response: %w", err)
	}
	if out.Data.Error != "" {
		return fmt.Errorf("PDW notify: %s", Clip(out.Data.Error, 200))
	}
	// PDW answers without an error when no device is registered or every device failed
	if out.Data.Report.Sent == 0 {
		return fmt.Errorf("PDW notify reached no device (%d registered)", out.Data.Report.Devices)
	}
	return nil
}
