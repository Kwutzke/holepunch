package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// Sigv4Proxy is an HTTP reverse proxy that signs requests with AWS sigv4.
// It accepts plain HTTP on the listen side and forwards signed HTTPS to the target.
type Sigv4Proxy struct {
	listenAddr string
	targetAddr string // the SSM tunnel endpoint (127.0.0.1:<port>)
	realHost   string // the real AWS hostname for signing
	awsProfile string
	awsRegion  string
	awsService string // e.g. "es" for OpenSearch
	listener   net.Listener
	server     *http.Server
	wg         sync.WaitGroup
}

// Sigv4Config holds configuration for creating a sigv4 signing proxy.
type Sigv4Config struct {
	ListenAddr string
	TargetAddr string // 127.0.0.1:<ssm_port>
	RealHost   string // the actual AWS endpoint hostname
	AWSProfile string
	AWSRegion  string
	AWSService string // "es" for OpenSearch/Elasticsearch Service
}

// NewSigv4Proxy creates a new signing reverse proxy.
func NewSigv4Proxy(cfg Sigv4Config) *Sigv4Proxy {
	return &Sigv4Proxy{
		listenAddr: cfg.ListenAddr,
		targetAddr: cfg.TargetAddr,
		realHost:   cfg.RealHost,
		awsProfile: cfg.AWSProfile,
		awsRegion:  cfg.AWSRegion,
		awsService: cfg.AWSService,
	}
}

// Start begins serving. Non-blocking.
func (p *Sigv4Proxy) Start(ctx context.Context) error {
	// Load AWS credentials from the configured profile.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithSharedConfigProfile(p.awsProfile),
		awsconfig.WithRegion(p.awsRegion),
	)
	if err != nil {
		return fmt.Errorf("loading AWS config for profile %s: %w", p.awsProfile, err)
	}

	targetURL := &url.URL{
		Scheme: "https",
		Host:   p.targetAddr,
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(targetURL)
	reverseProxy.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // SSM tunnel terminates TLS differently
			ServerName:         p.realHost,
		},
	}

	signer := v4.NewSigner()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Remove hop-by-hop and browser headers that the reverse proxy
		// modifies after signing, which invalidates the signature.
		r.Header.Del("Connection")
		r.Header.Del("Accept-Encoding")
		r.Header.Del("Accept-Language")
		r.Header.Del("Accept")
		r.Header.Del("Upgrade-Insecure-Requests")
		r.Header.Del("Cache-Control")
		r.Header.Del("Pragma")
		r.Header.Del("Sec-Fetch-Dest")
		r.Header.Del("Sec-Fetch-Mode")
		r.Header.Del("Sec-Fetch-Site")
		r.Header.Del("Sec-Fetch-User")
		r.Header.Del("Sec-Ch-Ua")
		r.Header.Del("Sec-Ch-Ua-Mobile")
		r.Header.Del("Sec-Ch-Ua-Platform")
		r.Header.Del("User-Agent")
		r.Header.Del("Referer")
		r.Header.Del("Origin")

		// Set the real host for the upstream request.
		r.Host = p.realHost

		// Read and buffer the body for signing.
		var bodyBytes []byte
		if r.Body != nil {
			bodyBytes, err = io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "failed to read request body", http.StatusInternalServerError)
				return
			}
			r.Body = io.NopCloser(bytesReader(bodyBytes))
		}

		// Compute payload hash.
		payloadHash := sha256Hash(bodyBytes)

		// Resolve credentials.
		creds, err := awsCfg.Credentials.Retrieve(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to get AWS credentials: %v", err), http.StatusInternalServerError)
			return
		}

		// Set the URL for signing (must use real host).
		r.URL.Host = p.realHost
		r.URL.Scheme = "https"

		// Sign the request.
		err = signer.SignHTTP(r.Context(), creds, r, payloadHash, p.awsService, p.awsRegion, time.Now())
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to sign request: %v", err), http.StatusInternalServerError)
			return
		}

		// Reset the body for the proxy to read.
		if bodyBytes != nil {
			r.Body = io.NopCloser(bytesReader(bodyBytes))
		}

		// Rewrite URL to target the SSM tunnel.
		r.URL.Host = p.targetAddr

		reverseProxy.ServeHTTP(w, r)
	})

	var lc net.ListenConfig
	p.listener, err = lc.Listen(ctx, "tcp", p.listenAddr)
	if err != nil {
		return fmt.Errorf("sigv4 proxy listen on %s: %w", p.listenAddr, err)
	}

	p.server = &http.Server{Handler: handler}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.server.Serve(p.listener)
	}()

	return nil
}

// Stop shuts down the signing proxy.
func (p *Sigv4Proxy) Stop() {
	if p.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		p.server.Shutdown(ctx)
	}
	p.wg.Wait()
}

// ListenAddr returns the address the proxy is listening on.
func (p *Sigv4Proxy) ListenAddr() string {
	if p.listener != nil {
		return p.listener.Addr().String()
	}
	return p.listenAddr
}

func sha256Hash(data []byte) string {
	h := sha256.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func bytesReader(b []byte) io.Reader {
	return &bytesReaderImpl{data: b, pos: 0}
}

type bytesReaderImpl struct {
	data []byte
	pos  int
}

func (r *bytesReaderImpl) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
