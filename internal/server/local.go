package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/jefflinse/slorp/internal/hub"
)

const (
	// thinks and plain are how the address a session reaches the local
	// model at says whether the model thinks.
	thinks = "think"
	plain  = "plain"
	// anyTemperature stands in that address for the model's own temperature.
	anyTemperature = "auto"
	// maxTemperature is the highest temperature a session may be given.
	maxTemperature = 2
	// localLimit is how large a request to the local model may be, in bytes.
	localLimit = 64 << 20
)

// localBase returns the address, under host, at which a session reaches the
// local model with thinking on or off and at temperature, nil for the
// model's own.
func localBase(host string, thinking bool, temperature *float64) string {
	mode, temp := plain, anyTemperature
	if thinking {
		mode = thinks
	}
	if temperature != nil {
		temp = strconv.FormatFloat(*temperature, 'f', -1, 64)
	}
	return "http://" + host + "/api/local/" + mode + "/" + temp
}

// localModel passes a session's requests on to the local model server,
// which speaks Anthropic's API beside its own. The address says how the
// model is to answer: what it is asked is given that temperature, and has
// thinking turned off unless the address leaves it on.
func localModel(h *hub.Hub) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode, temp := r.PathValue("thinking"), r.PathValue("temperature")
		var temperature *float64
		if temp != anyTemperature {
			t, err := strconv.ParseFloat(temp, 64)
			if err != nil || t < 0 || t > maxTemperature {
				http.NotFound(w, r)
				return
			}
			temperature = &t
		}
		if mode != thinks && mode != plain {
			http.NotFound(w, r)
			return
		}
		server, err := url.Parse(h.Summarizer().URL)
		if err != nil || server.Host == "" {
			http.Error(w, "Slorp is running without a local model", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.PathValue("rest"), "/messages") {
			asked, err := io.ReadAll(http.MaxBytesReader(w, r.Body, localLimit))
			if err != nil {
				http.Error(w, "request is too large", http.StatusRequestEntityTooLarge)
				return
			}
			asked = answeredAs(asked, mode == thinks, temperature)
			r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(asked)), int64(len(asked))
		}
		proxy := httputil.ReverseProxy{
			Rewrite: func(p *httputil.ProxyRequest) {
				p.SetURL(server)
				p.Out.URL.Path, p.Out.URL.RawPath = strings.TrimRight(server.Path, "/")+"/"+r.PathValue("rest"), ""
			},
			// What the model says is streamed, and is passed on as it comes.
			FlushInterval: -1,
		}
		proxy.ServeHTTP(w, r)
	})
}

// answeredAs returns the request asked of the model with temperature set,
// unless that is nil, and with thinking turned off unless thinking is true.
// A request that is not a JSON object is returned as it came.
func answeredAs(asked []byte, thinking bool, temperature *float64) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(asked, &fields) != nil || fields == nil {
		return asked
	}
	if temperature != nil {
		fields["temperature"] = json.RawMessage(strconv.FormatFloat(*temperature, 'f', -1, 64))
	}
	if !thinking {
		fields["thinking"] = json.RawMessage(`{"type":"disabled"}`)
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return asked
	}
	return out
}
