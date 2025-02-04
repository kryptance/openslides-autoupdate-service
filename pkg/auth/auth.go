// Package auth implement the auth system from the openslides-auth-service:
// https://github.com/OpenSlides/openslides-auth-service
package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/OpenSlides/openslides-autoupdate-service/internal/oserror"
	"github.com/OpenSlides/openslides-autoupdate-service/pkg/environment"
	"github.com/golang-jwt/jwt/v4"
	"github.com/ostcar/topic"

	"github.com/coreos/go-oidc"
)

// DebugTokenKey and DebugCookieKey are non random auth keys for development.
const (
	DebugTokenKey  = "auth-dev-token-key"
	DebugCookieKey = "auth-dev-cookie-key"
)

var (
	envAuthHost     = environment.NewVariable("KEYCLOAK_HOST", "localhost", "Host of the auth service.")
	envAuthPort     = environment.NewVariable("KEYCLOAK_PORT", "9004", "Port of the auth service.")
	envAuthProtocol = environment.NewVariable("KEYCLOAK_PROTOCOL", "http", "Protocol of the auth service.")
	envAuthFake     = environment.NewVariable("AUTH_FAKE", "false", "Use user id 1 for every request. Ignores all other auth environment variables.")

	envAuthTokenFile  = environment.NewVariable("AUTH_TOKEN_KEY_FILE", "/run/secrets/auth_token_key", "Key to sign the JWT auth tocken.")
	envAuthCookieFile = environment.NewVariable("AUTH_COOKIE_KEY_FILE", "/run/secrets/auth_cookie_key", "Key to sign the JWT auth cookie.")

	keycloakUrl                        = environment.NewVariable("OPENSLIDES_KEYCLOAK_URL", "", "The issuer of the token.")
	issuer                             = environment.NewVariable("OPENSLIDES_TOKEN_ISSUER", "", "The issuer of the token.")
	clientID                           = environment.NewVariable("OPENSLIDES_AUTH_CLIENT_ID", "", "The client ID of the application.")
	ctx                                = context.Background()
	oidcProvider *oidc.Provider        = nil
	verifier     *oidc.IDTokenVerifier = nil
)

type CustomTransport struct {
	Base        http.RoundTripper
	keycloakUrl string
}

func (t *CustomTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	keycloakUrl, _ := url.Parse(t.keycloakUrl)
	// Check if the request URL matches the .well-known path
	if strings.Contains(req.URL.Path, "/.well-known/openid-configuration") && strings.Contains(req.URL.Host, "localhost:8000") {
		// Modify the request to point to the new host and scheme
		req.URL.Scheme = keycloakUrl.Scheme
		req.URL.Host = keycloakUrl.Host
		fmt.Printf("Redirecting to: %s\n", req.URL.String())
	}

	// Use the base RoundTripper to perform the request
	return t.Base.RoundTrip(req)
}

// pruneTime defines how long a topic id will be valid. This should be higher
// then the max livetime of a token.
const pruneTime = 15 * time.Minute

const (
	authHeader = "Authentication"
)

func validateAccessToken(tokenString string) (*oidc.IDToken, error) {
	// Parse and verify the token using the verifier.
	idToken, err := verifier.Verify(ctx, tokenString)
	if err != nil {
		return nil, fmt.Errorf("failed to verify token: %v", err)
	}

	// Token is valid.
	return idToken, nil
}

// LogoutEventer tells, when a sessionID gets revoked.
//
// The method LogoutEvent has to block until there are new data. The returned
// data is a list of sessionIDs that are revoked.
type LogoutEventer interface {
	LogoutEvent(context.Context) ([]string, error)
}

// Auth authenticates a request against the openslides-auth-service.
//
// Has to be initialized with auth.New().
type Auth struct {
	fake bool

	logedoutSessions *topic.Topic[string]

	tokenKey  string
	cookieKey string
}

// New initializes the Auth object.
//
// Returns the initialized Auth objectand a function to be called in the
// background.
func New(lookup environment.Environmenter, messageBus LogoutEventer) (*Auth, func(context.Context, func(error)), error) {

	http.DefaultTransport = &CustomTransport{
		Base:        http.DefaultTransport,
		keycloakUrl: keycloakUrl.Value(lookup),
	}

	var err error

	var oidcProvider *oidc.Provider

	for {
		oidcProvider, err = oidc.NewProvider(ctx, issuer.Value(lookup))
		if err == nil {
			break
		}

		log.Println("Fehler beim Initialisieren des OIDC-Providers (%v). Neuer Versuch in 2s ...\n", err)
		time.Sleep(2 * time.Second)
	}

	// Set up the verifier using the discovered configuration.
	oidcConfig := &oidc.Config{
		ClientID: clientID.Value(lookup),
	}
	verifier = oidcProvider.Verifier(oidcConfig)

	fake, _ := strconv.ParseBool(envAuthFake.Value(lookup))

	authToken, err := environment.ReadSecretWithDefault(lookup, envAuthTokenFile, DebugTokenKey)
	if err != nil {
		return nil, nil, fmt.Errorf("reading auth token: %w", err)
	}

	cookieToken, err := environment.ReadSecretWithDefault(lookup, envAuthCookieFile, DebugCookieKey)
	if err != nil {
		return nil, nil, fmt.Errorf("reading cookie token: %w", err)
	}

	a := &Auth{
		fake:             fake,
		logedoutSessions: topic.New[string](),
		tokenKey:         authToken,
		cookieKey:        cookieToken,
	}

	// Make sure the topic is not empty
	a.logedoutSessions.Publish("")

	background := func(ctx context.Context, errorHandler func(error)) {
		if fake {
			return
		}

		go a.listenOnLogouts(ctx, messageBus, errorHandler)
		go a.pruneOldData(ctx)
	}

	return a, background, nil
}

// Authenticate uses the headers from the given request to get the user id. The
// returned context will be cancled, if the session is revoked.
func (a *Auth) Authenticate(w http.ResponseWriter, r *http.Request) (context.Context, error) {
	if a.fake {
		return r.Context(), nil
	}

	ctx := r.Context()

	p := new(OpenSlidesClaims)
	// 0 means anonymous user
	p.UserID = 0
	if err := a.loadToken(w, r, p); err != nil {
		return nil, fmt.Errorf("reading token: %w", err)
	}

	if p.UserID == 0 {
		return a.AuthenticatedContext(ctx, 0), nil
	}

	_, sessionIDs, err := a.logedoutSessions.Receive(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("getting already logged out sessions: %w", err)
	}
	for _, sid := range sessionIDs {
		if sid == p.SessionID {
			return nil, &authError{"invalid session", nil}
		}
	}

	userID := p.UserID
	ctx, cancelCtx := context.WithCancel(a.AuthenticatedContext(ctx, userID))

	println("Authenticated user: ", userID)

	go func() {
		defer cancelCtx()

		var cid uint64
		var sessionIDs []string
		var err error
		for {
			cid, sessionIDs, err = a.logedoutSessions.Receive(ctx, cid)
			if err != nil {
				return
			}

			for _, sid := range sessionIDs {
				if sid == p.SessionID {
					return
				}
			}
		}
	}()

	return ctx, nil
}

// AuthenticatedContext returns a new context that contains an userID.
//
// Should only used for internal URLs. All other URLs should use auth.Authenticate.
func (a *Auth) AuthenticatedContext(ctx context.Context, userID int) context.Context {
	return context.WithValue(ctx, userIDType, userID)
}

// FromContext returns the user id from a context returned by Authenticate().
//
// If the user is not logged in (public access) user 0 is returned.
//
// Panics, if the context was not returned from Authenticate
func (a *Auth) FromContext(ctx context.Context) int {
	if a.fake {
		return 1
	}

	v := ctx.Value(userIDType)
	if v == nil {
		panic("call to auth.FromContext() without auth.Authenticate()")
	}

	return v.(int)
}

// listenOnLogouts listen on logout events and closes the connections.
func (a *Auth) listenOnLogouts(ctx context.Context, logoutEventer LogoutEventer, errHandler func(error)) {
	if errHandler == nil {
		errHandler = func(error) {}
	}

	for {
		data, err := logoutEventer.LogoutEvent(ctx)
		if err != nil {
			if oserror.ContextDone(err) {
				return
			}

			errHandler(fmt.Errorf("receiving logout event: %w", err))
			time.Sleep(time.Second)
			continue
		}

		a.logedoutSessions.Publish(data...)
	}
}

// pruneOldData removes old logout events.
func (a *Auth) pruneOldData(ctx context.Context) {
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.logedoutSessions.Prune(time.Now().Add(-pruneTime))
		}
	}
}

// TrimPrefixCaseInsensitive trims the prefix from the string s. The prefix is
func TrimPrefixCaseInsensitive(s, prefix string) string {
	if strings.HasPrefix(strings.ToLower(s), strings.ToLower(prefix)) {
		return s[len(prefix):]
	}
	return s
}

// loadToken loads and validates the token. If the token is expired, it tries
// to renew it and writes the new token to the responsewriter.
func (a *Auth) loadToken(w http.ResponseWriter, r *http.Request, payload *OpenSlidesClaims) error {
	header := r.Header.Get(authHeader)

	encodedToken := TrimPrefixCaseInsensitive(header, "bearer ")

	if header == encodedToken {
		// No token. Handle the request as public access requst.
		return nil
	}

	token_validated, err := validateAccessToken(encodedToken)
	println("Token validated: ", token_validated)

	token, err := jwt.ParseWithClaims(encodedToken, payload, func(token *jwt.Token) (interface{}, error) {
		return []byte(a.tokenKey), nil
	})

	claims, _ := token.Claims.(*OpenSlidesClaims)
	fmt.Printf("UserID: %d\n", claims.UserID)
	//fmt.Printf("Issuer: %s\n", claims.Issuer)

	payload.UserID = claims.UserID

	if err != nil {
		var invalid *jwt.ValidationError
		if errors.As(err, &invalid) {
			return a.handleInvalidToken(r.Context(), invalid, w, encodedToken)
		}
	}

	//var claims OpenSlidesClaims
	//if err := token.Claims(&claims); err != nil {
	//	log.Fatalf("Failed to parse claims: %v", err)
	//}
	//
	//fmt.Printf("UserID: %s\n", payload.UserID)
	////fmt.Printf("Issuer: %s\n", claims.Issuer)

	payload.UserID = claims.UserID

	return nil
}

func (a *Auth) handleInvalidToken(ctx context.Context, invalid *jwt.ValidationError, w http.ResponseWriter, encodedToken string) error {
	if tokenExpired(invalid.Errors) {
		return authError{"auth token is expired", invalid}
	}

	return nil
}

func tokenExpired(errNo uint32) bool {
	return errNo&(jwt.ValidationErrorExpired|jwt.ValidationErrorNotValidYet) != 0
}

type authString string

const (
	userIDType authString = "user_id"
)

// OpenSlidesClaims custom openslides claims
type OpenSlidesClaims struct {
	jwt.StandardClaims
	UserID    int    `json:"os_uid"`
	SessionID string `json:"sid"`
}
