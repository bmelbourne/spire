package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/url"

	"github.com/go-jose/go-jose/v4"
	"github.com/gorilla/handlers"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/spire/pkg/common/cryptoutil"
	"github.com/spiffe/spire/pkg/common/telemetry"
)

const (
	keyUse = "sig"
)

type Handler struct {
	source              JWKSSource
	domainPolicy        DomainPolicy
	allowInsecureScheme bool
	setKeyUse           bool
	log                 logrus.FieldLogger
	jwtIssuer           *url.URL
	jwksURI             *url.URL
	serverPathPrefix    string

	http.Handler
}

func NewHandler(log logrus.FieldLogger, domainPolicy DomainPolicy, source JWKSSource, allowInsecureScheme bool, setKeyUse bool, jwtIssuer *url.URL, jwksURI *url.URL, serverPathPrefix string) *Handler {
	if serverPathPrefix == "" {
		serverPathPrefix = "/"
	}
	h := &Handler{
		domainPolicy:        domainPolicy,
		source:              source,
		allowInsecureScheme: allowInsecureScheme,
		setKeyUse:           setKeyUse,
		log:                 log,
		jwtIssuer:           jwtIssuer,
		jwksURI:             jwksURI,
		serverPathPrefix:    serverPathPrefix,
	}

	mux := http.NewServeMux()
	wkPath, err := url.JoinPath(serverPathPrefix, "/.well-known/openid-configuration")
	if err != nil {
		return nil
	}
	jwksPath, err := url.JoinPath(serverPathPrefix, "/keys")
	if err != nil {
		return nil
	}

	mux.Handle(wkPath, handlers.ProxyHeaders(http.HandlerFunc(h.serveWellKnown)))
	mux.Handle(jwksPath, http.HandlerFunc(h.serveKeys))

	h.Handler = mux
	return h
}

func (h *Handler) serveWellKnown(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	urlScheme := "https"
	if h.allowInsecureScheme && r.TLS == nil && r.URL.Scheme != "https" {
		urlScheme = "http"
	}

	issuerURL := h.jwtIssuer
	if h.jwtIssuer == nil {
		issuerURL = &url.URL{
			Scheme: urlScheme,
			Host:   r.Host,
		}
		if h.serverPathPrefix != "/" {
			issuerURL.Path = h.serverPathPrefix
		}
	}

	var jwksURI *url.URL
	switch {
	case h.jwksURI != nil:
		jwksURI = h.jwksURI
	case h.jwtIssuer != nil:
		// If jwksIsser is set but not jwksURI, fall back to 1.11.1 behavior until we can remove jwksIssuer leaking into jwksURI in 1.13.0
		keysPath, err := url.JoinPath(h.jwtIssuer.Path, "keys")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jwksURI = &url.URL{
			Scheme: h.jwtIssuer.Scheme,
			Host:   h.jwtIssuer.Host,
			Path:   keysPath,
		}
	default:
		keysPath, err := url.JoinPath(h.serverPathPrefix, "keys")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jwksURI = &url.URL{
			Scheme: urlScheme,
			Host:   r.Host,
			Path:   keysPath,
		}
	}

	if err := h.verifyHost(r.Host); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	doc := struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`

		// The following are required fields that we'll just hardcode response
		// to based on SPIRE capabilities, etc.
		AuthorizationEndpoint            string   `json:"authorization_endpoint"`
		ResponseTypesSupported           []string `json:"response_types_supported"`
		SubjectTypesSupported            []string `json:"subject_types_supported"`
		IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
	}{
		Issuer:  issuerURL.String(),
		JWKSURI: jwksURI.String(),

		AuthorizationEndpoint:            "",
		ResponseTypesSupported:           []string{"id_token"},
		SubjectTypesSupported:            []string{"public"},
		IDTokenSigningAlgValuesSupported: []string{"RS256", "ES256", "ES384"},
	}

	docBytes, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		http.Error(w, "failed to marshal document", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(docBytes)
}

func (h *Handler) serveKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jwks, modTime, ok := h.source.FetchKeySet()
	if !ok {
		http.Error(w, "document not available", http.StatusInternalServerError)
		return
	}

	jwks.Keys = h.enrichJwksKeys(jwks.Keys)

	jwksBytes, err := json.MarshalIndent(jwks, "", "  ")
	if err != nil {
		http.Error(w, "failed to marshal JWKS", http.StatusInternalServerError)
		return
	}

	// Disable caching
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	w.Header().Set("Content-Type", "application/json")
	http.ServeContent(w, r, "keys", modTime, bytes.NewReader(jwksBytes))
}

func (h *Handler) verifyHost(host string) error {
	// Obtain the domain name from the host value, which comes from the
	// request, or is pulled from the X-Forwarded-Host header (via the
	// ProxyHeaders middleware). The value may be in host or host:port form.
	domain, _, err := net.SplitHostPort(host)
	if err != nil {
		// `Host` was not in the host:port form.
		domain = host
	}
	return h.domainPolicy(domain)
}

func (h *Handler) enrichJwksKeys(jwkKeys []jose.JSONWebKey) []jose.JSONWebKey {
	if h.setKeyUse {
		for i := range jwkKeys {
			jwkKeys[i].Use = keyUse
		}
	}
	for i, k := range jwkKeys {
		alg, err := cryptoutil.JoseAlgFromPublicKey(k.Key)
		if err != nil {
			h.log.WithFields(logrus.Fields{
				telemetry.Kid: k.KeyID,
			}).WithError(err).Errorf("Failed to get public key algorithm")
		}
		jwkKeys[i].Algorithm = string(alg)
	}
	return jwkKeys
}
