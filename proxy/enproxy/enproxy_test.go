package enproxy

import (
	"bufio"
	"fmt"
	"github.com/enfabrica/enkit/lib/khttp/krequest"
	"github.com/enfabrica/enkit/lib/khttp/ktest"
	"github.com/enfabrica/enkit/lib/khttp/protocol"
	"github.com/enfabrica/enkit/lib/knetwork/echo"
	"github.com/enfabrica/enkit/lib/logger"
	"github.com/enfabrica/enkit/lib/oauth"
	"github.com/enfabrica/enkit/lib/token"
	"github.com/enfabrica/enkit/proxy/httpp"
	"github.com/enfabrica/enkit/proxy/nasshp"
	"github.com/enfabrica/enkit/proxy/ptunnel"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Deny returns an authenticator that either denies a request, or returns a constant cookie.
// Every request received is logged in log.
func Deny(cookie *oauth.CredentialsCookie, urls []string, log *[]string) oauth.Authenticate {
	return func(w http.ResponseWriter, r *http.Request, rurl *url.URL) (*oauth.CredentialsCookie, error) {
		uri := *r.URL
		if uri.Host == "" {
			uri.Host = r.Host
		}
		suri := uri.String()

		if log != nil {
			(*log) = append(*log, suri)
		}

		for _, block := range urls {
			if strings.HasPrefix(suri, block) {
				http.Error(w, "Not authorized", http.StatusUnauthorized)
				return nil, nil
			}
		}

		return cookie, nil
	}
}

func Allow(cookie *oauth.CredentialsCookie) oauth.Authenticate {
	return func(w http.ResponseWriter, r *http.Request, rurl *url.URL) (*oauth.CredentialsCookie, error) {
		return cookie, nil
	}
}

// Server creates a Starter capable of binding an unused port and start an http server on it.
func Server(wg *sync.WaitGroup, url *string) Starter {
	wg.Add(1)
	return func(log logger.Printer, handler http.Handler, domains ...string) error {
		defer wg.Done()
		var err error
		*url, err = ktest.Start(handler)
		return err
	}
}

func TestInvalidConfig(t *testing.T) {
	var url string
	rng := rand.New(rand.NewSource(1))

	// Config file without any mappings.
	ep, err := New(rng, WithHttpStarter(Server(&sync.WaitGroup{}, &url)))
	assert.Regexp(t, "config file.*has no Mapping.*defined", err)
	assert.Nil(t, ep)

	config := Config{
		Mapping: []httpp.Mapping{
			httpp.Mapping{
				From: httpp.HostPath{
					Host: "test.lan",
					Path: "/",
				},
				To: "toast.lan"},
		},
	}

	// One mapping is provided, now authentication is required.
	ep, err = New(rng, WithHttpStarter(Server(&sync.WaitGroup{}, &url)), WithConfig(config))
	assert.Regexp(t, "error in mapping entry 0", err)
	assert.Nil(t, ep)

	// Valid, but there is no tunnel configuration nor authentication, it should spew a few warnings.
	accumulator := logger.NewAccumulator()
	config.Mapping[0].Auth = httpp.MappingPublic
	ep, err = New(rng, WithHttpStarter(Server(&sync.WaitGroup{}, &url)), WithConfig(config), WithLogging(accumulator))
	assert.NoError(t, err)
	assert.NotNil(t, ep)

	events := accumulator.Retrieve()
	assert.True(t, len(events) >= 5, "%v", events)
}

func TestSimpleHTTP(t *testing.T) {
	// Create a few http servers to use as backends.
	s1, err := ktest.Start(http.HandlerFunc(ktest.StringHandler("s1")))
	assert.Nil(t, err)
	s2, err := ktest.Start(http.HandlerFunc(ktest.StringHandler("s2")))
	assert.Nil(t, err)
	s3, err := ktest.Start(http.HandlerFunc(ktest.StringHandler("s3")))
	assert.Nil(t, err)
	s4, err := ktest.Start(http.HandlerFunc(ktest.StringHandler("s4")))
	assert.Nil(t, err)

	// Frontend proxy config.
	config := Config{
		Mapping: []httpp.Mapping{
			// A single file path on this host.
			httpp.Mapping{
				From: httpp.HostPath{
					Host: "test1.lan",
					Path: "/glad",
				},
				Auth: httpp.MappingPublic,
				To:   s1,
			},

			// Multiple overlapping paths on test2.
			httpp.Mapping{
				From: httpp.HostPath{
					Host: "test2.lan",
					Path: "/",
				},
				Auth: httpp.MappingPublic,
				To:   s2,
			},

			// ... this one is private (but a directory).
			httpp.Mapping{
				From: httpp.HostPath{
					Host: "test2.lan",
					Path: "/oppose/",
				},
				To: s3,
			},

			// ... this one is also private - but access will be denied.
			httpp.Mapping{
				From: httpp.HostPath{
					Host: "test2.lan",
					Path: "/deny/",
				},
				To: s3,
			},

			// ... this one is a prefix of /oppose and public.
			httpp.Mapping{
				From: httpp.HostPath{
					Host: "test2.lan",
					Path: "/opp/",
				},
				Auth: httpp.MappingPublic,
				To:   s4,
			},

			// No wildcard match for now.
		},

		// Allow any tunnel.
		Tunnels: []string{"*"},
	}

	cookie := &oauth.CredentialsCookie{
		Identity: oauth.Identity{
			Id:           "id",
			Username:     "username",
			Organization: "organization",
		},
	}

	rng := rand.New(rand.NewSource(1))

	var fe string
	var metrics string
	var wg sync.WaitGroup
	reg := prometheus.NewRegistry()
	accessLog := []string{}
	accumulator := logger.NewAccumulator()
	ep, err := New(rng, WithHttpStarter(Server(&wg, &fe)), WithConfig(config), WithMetricsStarter(Server(&wg, &metrics)),
		WithLogging(accumulator), WithAuthenticator(Deny(cookie, []string{"//test2.lan/deny"}, &accessLog)),
		WithNasshpMods(nasshp.WithSymmetricOptions(token.WithGeneratedSymmetricKey(0))),
		WithPrometheus(reg, reg))
	assert.NoError(t, err)
	assert.NotNil(t, ep)

	err = ep.Run()
	assert.NoError(t, err)
	wg.Wait()

	var herr *protocol.HTTPError
	body := ""
	metrics += "/metrics"

	// The root fe for test1.lan is not mapped anywhere, should return an error.
	err = protocol.Get(fe, protocol.Read(protocol.String(&body)), protocol.WithRequestOptions(krequest.SetHost("test1.lan")))
	assert.ErrorAs(t, err, &herr)
	assert.Equal(t, http.StatusNotFound, herr.Resp.StatusCode)

	// /glad for test1.lan is mapped to s1, let's check that.
	err = protocol.Get(fe+"glad", protocol.Read(protocol.String(&body)), protocol.WithRequestOptions(krequest.SetHost("test1.lan")))
	assert.NoError(t, err)
	assert.Equal(t, "s1", body)
	// /glad should be an exact match, so /gladder should not match.
	err = protocol.Get(fe+"gladder", protocol.Read(protocol.String(&body)), protocol.WithRequestOptions(krequest.SetHost("test1.lan")))
	assert.ErrorAs(t, err, &herr)
	assert.Equal(t, http.StatusNotFound, herr.Resp.StatusCode)
	// /glad/glod should also not work, as /glad was not configured as a path prefix (not /glad/).
	err = protocol.Get(fe+"glad/glod", protocol.Read(protocol.String(&body)), protocol.WithRequestOptions(krequest.SetHost("test1.lan")))
	assert.ErrorAs(t, err, &herr)
	assert.Equal(t, http.StatusNotFound, herr.Resp.StatusCode)

	// Let's try a single request to test2.lan root.
	err = protocol.Get(fe, protocol.Read(protocol.String(&body)), protocol.WithRequestOptions(krequest.SetHost("test2.lan")))
	assert.NoError(t, err)
	assert.Equal(t, "s2", body)
	// test2.lan maps all prefixes to s2, as it has a default path. Let's test it.
	err = protocol.Get(fe+"darwin/was/right", protocol.Read(protocol.String(&body)), protocol.WithRequestOptions(krequest.SetHost("test2.lan")))
	assert.NoError(t, err)
	assert.Equal(t, "s2", body)

	// Before making any private request, let's ensure no private request was made so far.
	assert.Equal(t, 0, len(accessLog))

	// Private request, should be allowed, but checked with the authenticator.
	// Note that this verifies both that the map works correctly, and that authentication is enforced.
	err = protocol.Get(fe+"oppose", protocol.Read(protocol.String(&body)), protocol.WithRequestOptions(krequest.SetHost("test2.lan")))
	assert.NoError(t, err)
	assert.Equal(t, "s3", body)
	assert.Equal(t, "//test2.lan/oppose", accessLog[len(accessLog)-1])
	// Same for subdirectories.
	err = protocol.Get(fe+"oppose/censorship", protocol.Read(protocol.String(&body)), protocol.WithRequestOptions(krequest.SetHost("test2.lan")))
	assert.NoError(t, err)
	assert.Equal(t, "s3", body)
	assert.Equal(t, "//test2.lan/oppose/censorship", accessLog[len(accessLog)-1])

	// Let's see what happens if the authentication layer denies a request.
	err = protocol.Get(fe+"deny/oppression", protocol.Read(protocol.String(&body)), protocol.WithRequestOptions(krequest.SetHost("test2.lan")))
	assert.ErrorAs(t, err, &herr)
	assert.Equal(t, http.StatusUnauthorized, herr.Resp.StatusCode)
	assert.Equal(t, "//test2.lan/deny/oppression", accessLog[len(accessLog)-1])

	// /opp is a prefix of /oppose, but should still work as expected.
	err = protocol.Get(fe+"opp", protocol.Read(protocol.String(&body)), protocol.WithRequestOptions(krequest.SetHost("test2.lan")))
	assert.NoError(t, err)
	assert.Equal(t, "s4", body)

	// Start an echo server to use as a tunnel backend.
	e, err := echo.New("127.0.0.1:0")
	assert.NoError(t, err)
	assert.NotNil(t, e)

	echoaddress, err := e.Address()
	assert.NoError(t, err)

	defer e.Close()
	go e.Run()

	proxy, err := url.Parse(fe)
	assert.NoError(t, err)

	// Try a tunnel connection.
	pool := nasshp.NewBufferPool(8192)
	tlog := logger.NewAccumulator()
	tunnel, err := ptunnel.NewTunnel(pool, ptunnel.WithLogger(tlog))
	assert.NoError(t, err)
	assert.NotNil(t, tunnel)

	defer tunnel.Close()
	go tunnel.KeepConnected(proxy, echoaddress.IP.String(), uint16(echoaddress.Port))

	send, write := io.Pipe()
	go tunnel.Send(send)

	read, receive := io.Pipe()
	go tunnel.Receive(receive)

	quote := "You never change things by fighting the existing reality. To change something, build a new model that makes the existing model obsolete.\n"
	l, err := write.Write([]byte(quote))
	assert.NoError(t, err)
	assert.Equal(t, len(quote), l)

	rback, err := bufio.NewReader(read).ReadString('\n')
	assert.NoError(t, err)
	assert.Equal(t, quote, rback)

	assert.Nil(t, tlog.Retrieve())

	// This is for defense in depth: check that the test actually connected to the echo server VIA THE PROXY.
	// We do so by verifying that there is a log entry reporting the connection.
	// TODO: once we have better metrics and introspection, do something smarter.
	events := accumulator.Retrieve()
	assert.True(t, len(events) > 1)
	assert.Regexp(t, "- connects "+echoaddress.String(), events[len(events)-1].Message)

	err = protocol.Get(metrics, protocol.Read(protocol.String(&body)))
	assert.NoError(t, err)
	lines := strings.Split(body, "\n")
	// Surely there are more than 10 metrics...
	assert.True(t, len(lines) > 10)
	// Check that all metrics are expected...
	assert.Regexp(t, "(?m)^(#|nasshp_)", body, "%s", body)
}

// Generate metrics through a prometheus PedanticRegistry, so that it will
// report errors like conflicting metric names, incorrect representations, and such.
func TestPedanticMetrics(t *testing.T) {
	// Create a few http servers to use as backends.
	s1, err := ktest.Start(http.HandlerFunc(ktest.StringHandler("s1")))
	assert.Nil(t, err)

	// Simple proxy config.
	config := Config{
		Mapping: []httpp.Mapping{
			// A single file path on this host.
			httpp.Mapping{
				From: httpp.HostPath{
					Host: "test1.lan",
					Path: "/glad",
				},
				Auth: httpp.MappingPublic,
				To:   s1,
			},
		},

		// Allow any tunnel.
		Tunnels: []string{"*"},
	}

	cookie := &oauth.CredentialsCookie{
		Identity: oauth.Identity{
			Id:           "id",
			Username:     "username",
			Organization: "organization",
		},
	}

	var proxy string
	var metrics string
	var wg sync.WaitGroup
	rng := rand.New(rand.NewSource(1))

	// Ensures that no other variables are registered (eg, golang defaults) and...
	// errors out in case there are inconsistencies in the declared variables.
	reg := prometheus.NewPedanticRegistry()
	accumulator := logger.NewAccumulator()
	ep, err := New(rng, WithHttpStarter(Server(&wg, &proxy)), WithMetricsStarter(Server(&wg, &metrics)),
		WithConfig(config), WithLogging(accumulator), WithAuthenticator(Deny(cookie, nil, nil)),
		WithNasshpMods(nasshp.WithSymmetricOptions(token.WithGeneratedSymmetricKey(0))),
		WithPrometheus(reg, reg))
	assert.NoError(t, err)
	assert.NotNil(t, ep)

	err = ep.Run()
	assert.NoError(t, err)
	wg.Wait()

	metrics += "/metrics"
	body := ""

	// The root fe for test1.lan is not mapped anywhere, should return an error.
	err = protocol.Get(metrics, protocol.Read(protocol.String(&body)))
	assert.NoError(t, err, "%s - %r", err, accumulator.Retrieve())
	lines := strings.Split(body, "\n")
	// Surely there are more than 10 metrics...
	assert.True(t, len(lines) > 10)
	// Check that all metrics are expected...
	assert.Regexp(t, "(?m)^(#|nasshp_)", body, "%s", body)
}

func TestBandwidth(t *testing.T) {
	config := Config{
		Mapping: []httpp.Mapping{
			httpp.Mapping{
				From: httpp.HostPath{
					Host: "",
					Path: "/",
				},
				Auth: httpp.MappingPublic,
			},
		},

		// Allow any tunnel.
		Tunnels: []string{"*"},
	}

	cookie := &oauth.CredentialsCookie{
		Identity: oauth.Identity{
			Id:           "id",
			Username:     "username",
			Organization: "organization",
		},
	}

	var fe string
	rng := rand.New(rand.NewSource(1))

	accumulator := logger.NewAccumulator()
	ep, err := New(rng, WithHttpStarter(Server(&sync.WaitGroup{}, &fe)), WithConfig(config),
		WithLogging(accumulator), WithAuthenticator(Allow(cookie)),
		WithNasshpMods(nasshp.WithSymmetricOptions(token.WithGeneratedSymmetricKey(0))))
	assert.NoError(t, err)
	assert.NotNil(t, ep)

	ep.Run()

	// Start an echo server to use as a tunnel backend.
	e, err := echo.New("127.0.0.1:0")
	assert.NoError(t, err)
	assert.NotNil(t, e)

	echoaddress, err := e.Address()
	assert.NoError(t, err)

	defer e.Close()
	go e.Run()

	proxy, err := url.Parse(fe)
	assert.NoError(t, err)

	// Try a tunnel connection.
	pool := nasshp.NewBufferPool(8192)
	tlog := logger.NewAccumulator()
	tunnel, err := ptunnel.NewTunnel(pool, ptunnel.WithLogger(tlog))
	assert.NoError(t, err)
	assert.NotNil(t, tunnel)

	defer tunnel.Close()
	go tunnel.KeepConnected(proxy, echoaddress.IP.String(), uint16(echoaddress.Port))

	send, write := io.Pipe()
	go func() {
		tunnel.Send(send)
		tunnel.Close()
		send.Close()
	}()

	read, receive := io.Pipe()
	go func() {
		tunnel.Receive(receive)
		tunnel.Close()
		receive.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	const kTotalBytes = 10 * 1048576
	quote := "You never change things by fighting the existing reality. To change something, build a new model that makes the existing model obsolete.\n"

	start := time.Now()
	go func() {
		defer wg.Done()

		transferred := 0
		for count := 0; transferred < kTotalBytes; count++ {
			l, err := write.Write([]byte(fmt.Sprintf("%05d %s", count, quote)))
			assert.NoError(t, err)
			assert.Equal(t, len(quote)+6, l)
			transferred += l
		}
		write.Close()
	}()

	go func() {
		defer wg.Done()

		reader := bufio.NewReader(read)
		transferred := 0
		for count := 0; transferred < kTotalBytes; count++ {
			rback, err := reader.ReadString('\n')
			if err == io.EOF {
				break
			}
			assert.NoError(t, err)
			assert.Equal(t, fmt.Sprintf("%05d %s", count, quote), rback, "incorrect at offset %d - count %d", transferred, count)
			transferred += len(rback)
		}
		read.Close()
	}()
	wg.Wait()

	done := time.Now()
	logs := tlog.Retrieve()
	// Should only log the fact that the library quit.
	assert.True(t, len(logs) <= 0, "more than one log entry: %v", logs)

	delta := done.Sub(start)
	rate := (kTotalBytes / delta.Seconds()) / 1024
	assert.True(t, rate >= 10, "total run time: %s - rate %f KBps", delta, rate)
}
