package main

import (
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"github.com/jamiealquiza/bicache"
	"golang.org/x/crypto/blake2b"
)

const userAgent = "gitlab-source-link-proxy"

// NB GitLab 19.0 removed the OAuth 2.0 Resource Owner Password Credentials
//    flow (grant_type=password), so we can no longer exchange the basic
//    authentication username and password for an access token. Instead, the
//    basic authentication password must be a GitLab access token (e.g. a
//    personal access token), which we forward as a Bearer token.
//    See https://docs.gitlab.com/api/oauth2/

var ErrInvalidToken = errors.New("invalid access token")

type UserResponse struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// GetTokenUser returns the GitLab user that owns the given access token.
// see https://docs.gitlab.com/api/users/#list-current-user
func GetTokenUser(gitLabUserURL, accessToken string) (*UserResponse, error) {
	request, err := http.NewRequest(http.MethodGet, gitLabUserURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	// NB we never dump the response, as it might contain sensitive data.
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrInvalidToken
	default:
		return nil, fmt.Errorf("invalid response status %s", response.Status)
	}
	// TODO check content-type.
	var userResponse UserResponse
	if err := json.Unmarshal(responseBody, &userResponse); err != nil {
		return nil, err
	}
	if userResponse.Username == "" {
		return nil, ErrInvalidToken
	}
	return &userResponse, nil
}

// GetCachedTokenUser is like GetTokenUser but caches the result for an hour.
// NB the cache is keyed by the access token hash, so the access token itself is never stored.
func GetCachedTokenUser(c *bicache.Bicache, gitLabUserURL, accessToken string) (string, error) {
	h := blake2b.Sum256([]byte(accessToken))
	k := hex.EncodeToString(h[:])
	v := c.Get(k)
	if v != nil {
		log.Printf("Cache-Hit validating the access token of the %s user", v.(string))
		return v.(string), nil
	}
	log.Print("Cache-Miss validating an access token")
	userResponse, err := GetTokenUser(gitLabUserURL, accessToken)
	if err != nil {
		return "", err
	}
	c.SetTTL(k, userResponse.Username, 3600)
	return userResponse.Username, nil
}

// dumpRequest dumps the request headers with the credentials redacted.
func dumpRequest(r *http.Request) string {
	c := r.Clone(r.Context())
	if c.Header.Get("Authorization") != "" {
		c.Header.Set("Authorization", "REDACTED")
	}
	dump, _ := httputil.DumpRequest(c, false)
	return string(dump)
}

func requestAuthentication(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="GitLab"`)
	w.Header().Set("Cache-Control", `no-cache`)
	http.Error(w, "HTTP Basic: Access denied", http.StatusUnauthorized)
}

var (
	version = "unknown"
	commit  = "unknown"
	date    = "unknown"
)

var (
	listenAddressFlag      = flag.String("listen-address", "127.0.0.1:7000", "HOSTNAME:PORT where this http proxy listens at (e.g. 127.0.0.1:7000)")
	baseGitLabURLFlag      = flag.String("gitlab-base-url", "", "GitLab Base URL (e.g. https://gitlab.example.com/)")
	insecureSkipVerifyFlag = flag.Bool("tls-insecure-skip-verify", false, "Skip GitLab TLS verification")
	validateTokenFlag      = flag.Bool("validate-token", true, "Validate the given access token before proxying the request")
)

func main() {
	flag.Parse()

	log.Printf("Starting gitlab-source-link-proxy (version %s; commit %s; date %s)", version, commit, date)

	if *baseGitLabURLFlag == "" {
		log.Printf("Usage of %s:\n", os.Args[0])
		flag.PrintDefaults()
		return
	}

	if *insecureSkipVerifyFlag {
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	c, err := bicache.New(&bicache.Config{
		MFUSize:    24,        // MFU capacity in keys
		MRUSize:    64,        // MRU capacity in keys
		ShardCount: 64,        // Shard count. Defaults to 512 if unset.
		AutoEvict:  60 * 1000, // Run TTL evictions + MRU->MFU promotions / evictions automatically every 60s.
		EvictLog:   true,      // Emit eviction timing logs.
		NoOverflow: false,     // Disallow Set ops when the MRU cache is full.
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	gitLabBaseURL, err := url.Parse(strings.TrimRight(*baseGitLabURLFlag, "/"))
	if err != nil {
		log.Fatal(err)
	}
	gitLabUserURL := gitLabBaseURL.String() + "/api/v4/user"

	reverseProxy := httputil.NewSingleHostReverseProxy(gitLabBaseURL)
	defaultReverseProxyDirector := reverseProxy.Director
	reverseProxy.Director = func(r *http.Request) {
		defaultReverseProxyDirector(r)
		r.Header.Set("User-Agent", userAgent)
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%q", dumpRequest(r))
		// NB the basic authentication password must be a GitLab access token
		//    (e.g. a personal access token); the username is ignored.
		username, accessToken, ok := r.BasicAuth()
		if !ok || accessToken == "" {
			log.Print("request not authenticated, requesting authentication")
			requestAuthentication(w)
			return
		}
		if *validateTokenFlag {
			tokenUsername, err := GetCachedTokenUser(c, gitLabUserURL, accessToken)
			if err != nil {
				log.Printf("Error validating the access token given as the password of the %q user: %v", username, err)
				requestAuthentication(w)
				return
			}
			log.Printf("Authenticated as the %s user", tokenUsername)
		}
		r.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))
		reverseProxy.ServeHTTP(w, r)
	})
	log.Fatal(http.ListenAndServe(*listenAddressFlag, nil))
}
