package devbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Keys is a dev box's key to the server, kept in File. A box with no key joins an account: it asks
// the server, shows (logs) a code, and once someone signed in enters it on the server's account
// page, collects its key and keeps it. A key the server refuses is forgotten, so the box joins again.
type Keys struct {
	File, Server, Box string
	HTTP              *http.Client
	Logf              func(format string, args ...any)
	// ServerVersion is called with the server's version after each request, so a box that is
	// still joining follows the server's version too.
	ServerVersion func(v string)
	RetryEvery    time.Duration // after a failed request; default 5s

	mu sync.Mutex
}

// collectWait is how long one request for the key waits on the server for the code to be entered.
const collectWait = 50 * time.Second

func (k *Keys) defaults() {
	if k.HTTP == nil {
		k.HTTP = &http.Client{Timeout: collectWait + 20*time.Second}
	}
	if k.Logf == nil {
		k.Logf = log.Printf
	}
	if k.RetryEvery == 0 {
		k.RetryEvery = 5 * time.Second
	}
}

// Key is the box's key: from File, else from joining an account, which waits until someone enters
// the box's code (or ctx ends).
func (k *Keys) Key(ctx context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.defaults()
	b, err := os.ReadFile(k.File)
	if key := strings.TrimSpace(string(b)); err == nil && key != "" {
		return key, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	key, err := k.join(ctx)
	if err != nil {
		return "", err
	}
	if err := writeKey(k.File, key); err != nil {
		return "", fmt.Errorf("keep the key: %w", err)
	}
	k.Logf("joined the account: key kept in %s", k.File)
	return key, nil
}

// Refused forgets key, which the server refused (the box was removed from its account), so the
// next Key joins again. A key the file no longer holds is left alone.
func (k *Keys) Refused(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if b, err := os.ReadFile(k.File); err == nil && strings.TrimSpace(string(b)) == key {
		_ = os.Remove(k.File)
	}
}

func writeKey(file, key string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := file + ".new"
	if err := os.WriteFile(tmp, []byte(key+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

type enrollment struct {
	ID        string    `json:"id"`
	Code      string    `json:"code"`
	Secret    string    `json:"secret"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// errExpired: the enrollment's code was not entered in time.
var errExpired = errors.New("the code expired")

func (k *Keys) join(ctx context.Context) (string, error) {
	for {
		e, err := k.enroll(ctx)
		for err == nil {
			var key string
			if key, err = k.collect(ctx, e); err == nil && key != "" {
				return key, nil
			}
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !errors.Is(err, errExpired) {
			k.Logf("joining an account: %v", err)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(k.RetryEvery):
			}
		}
	}
}

func (k *Keys) url(path string) string { return strings.TrimRight(k.Server, "/") + path }

func (k *Keys) do(ctx context.Context, req *http.Request) (*http.Response, []byte, error) {
	resp, err := k.HTTP.Do(req.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if k.ServerVersion != nil {
		k.ServerVersion(resp.Header.Get(VersionHeader))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp, body, err
}

func (k *Keys) enroll(ctx context.Context) (enrollment, error) {
	body, _ := json.Marshal(map[string]string{"box": k.Box})
	req, err := http.NewRequest(http.MethodPost, k.url("/api/devbox-enrollments"), bytes.NewReader(body))
	if err != nil {
		return enrollment{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, b, err := k.do(ctx, req)
	if err != nil {
		return enrollment{}, err
	}
	var e enrollment
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(b, &e) != nil || e.Code == "" {
		return enrollment{}, fmt.Errorf("server refused: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	k.Logf("this dev box (%s) is in no account: to add it, sign in at %s/account and enter the code %s (until %s)",
		k.Box, strings.TrimRight(k.Server, "/"), e.Code, e.ExpiresAt.Local().Format("15:04 MST"))
	return e, nil
}

// collect is the key once the code is entered, "" while it is not yet, or errExpired.
func (k *Keys) collect(ctx context.Context, e enrollment) (string, error) {
	req, err := http.NewRequest(http.MethodPost, k.url("/api/devbox-enrollments/"+e.ID+"/key?wait="+collectWait.String()), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+e.Secret)
	resp, b, err := k.do(ctx, req)
	if err != nil {
		return "", err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var got struct{ Key string }
		if err := json.Unmarshal(b, &got); err != nil || got.Key == "" {
			return "", fmt.Errorf("no key in the server's answer: %v", err)
		}
		return got.Key, nil
	case http.StatusAccepted:
		return "", nil
	case http.StatusNotFound:
		return "", errExpired
	}
	return "", fmt.Errorf("server refused: %s: %s", resp.Status, strings.TrimSpace(string(b)))
}
