// capturefault is a dedicated-host verification proxy, never a runtime service.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/helmrdotdev/helmr/internal/cas"
)

type fault struct {
	mu              sync.Mutex
	delay           time.Duration
	armed           bool
	started, failed time.Time
	key             string
	forward         http.Handler
}

func (f *fault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if r.URL.Path == "/__fault" {
		defer f.mu.Unlock()
		if r.Method == http.MethodPost {
			if f.armed || !f.started.IsZero() {
				http.Error(w, "fault already armed", 409)
				return
			}
			f.armed = true
		} else if r.Method != http.MethodGet {
			http.Error(w, "method", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"armed": f.armed, "started": f.started, "failed": f.failed, "key": f.key, "delay_ms": f.delay.Milliseconds()})
		return
	}
	selected := f.armed && r.Header.Get("Content-Type") == cas.CheckpointMemoryMediaType &&
		(r.Method == http.MethodPut || (r.Method == http.MethodPost && r.URL.Query().Has("uploads")))
	if selected {
		f.armed = false
		f.started = time.Now().UTC()
		f.key = r.URL.Path
	}
	f.mu.Unlock()
	if !selected {
		f.forward.ServeHTTP(w, r)
		return
	}
	timer := time.NewTimer(f.delay)
	defer timer.Stop()
	select {
	case <-r.Context().Done():
		return
	case <-timer.C:
	}
	f.mu.Lock()
	f.failed = time.Now().UTC()
	f.mu.Unlock()
	// A non-retryable S3 error fails this upload, not a later unrelated capture.
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>Dedicated capture failure case</Message></Error>`))
}

type signedTransport struct {
	credentials aws.CredentialsProvider
	region      string
}

func (t signedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.sign(r); err != nil {
		return nil, err
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (t signedTransport) sign(r *http.Request) error {
	credentials, err := t.credentials.Retrieve(r.Context())
	if err != nil {
		return err
	}
	r.Header.Del("Authorization")
	r.Header.Del("X-Amz-Security-Token")
	query := r.URL.Query()
	for key := range query {
		switch strings.ToLower(key) {
		case "x-amz-algorithm", "x-amz-credential", "x-amz-date", "x-amz-expires", "x-amz-signedheaders", "x-amz-signature", "x-amz-security-token":
			query.Del(key)
		}
	}
	r.URL.RawQuery = query.Encode()
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if payload == "" {
		payload = "UNSIGNED-PAYLOAD"
	}
	r.Header.Set("X-Amz-Content-Sha256", payload)
	return v4.NewSigner(func(options *v4.SignerOptions) {
		options.DisableURIPathEscaping = true
	}).SignHTTP(r.Context(), credentials, r, payload, "s3", t.region, time.Now())
}

func bucketHandler(bucket string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__fault" && r.URL.Path != "/"+bucket && !strings.HasPrefix(r.URL.Path, "/"+bucket+"/") {
			http.Error(w, "outside dedicated bucket", 403)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	listen := flag.String("listen", "127.0.0.1:58089", "loopback endpoint for this isolated profile")
	bucket := flag.String("bucket", "", "exact dedicated CAS bucket")
	region := flag.String("region", "", "AWS region of the dedicated bucket")
	hold := flag.Duration("hold", 360*time.Second, "delay before failing the first armed memory upload")
	flag.Parse()
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || *bucket == "" || strings.ContainsAny(*bucket, "/:") || *region == "" || strings.ContainsAny(*region, "/:") || *hold <= 0 || *hold > 10*time.Minute {
		log.Fatal("require loopback, bucket, region and hold in (0,10m]")
	}
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(*region))
	if err != nil {
		log.Fatal(err)
	}
	proxy := &httputil.ReverseProxy{Rewrite: func(p *httputil.ProxyRequest) {
		p.Out.URL.Scheme = "https"
		p.Out.URL.Host = fmt.Sprintf("s3.%s.amazonaws.com", *region)
		p.Out.Host = p.Out.URL.Host
	}, Transport: signedTransport{cfg.Credentials, *region}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) { http.Error(w, "S3 forwarding failed", 502) }}
	f := &fault{delay: *hold, forward: proxy}
	handler := bucketHandler(*bucket, f)
	log.Printf("capture verification proxy listening on %s", *listen)
	log.Fatal((&http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
}
