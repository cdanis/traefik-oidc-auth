package traefik_oidc_auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type TraefikOidcAuth struct {
	next              http.Handler
	ProviderURL       *url.URL
	Config            *Config
	DiscoveryDocument *OidcDiscovery
	Jwks              *JwksHandler
}

func (toa *TraefikOidcAuth) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if strings.HasPrefix(req.RequestURI, toa.Config.CallbackUri) {
		toa.handleCallback(rw, req)
		return
	}

	if toa.Config.LoginUri != "" && strings.HasPrefix(req.RequestURI, toa.Config.LoginUri) {
		toa.redirectToProvider(rw, req)
		return
	}

	if strings.HasPrefix(req.RequestURI, toa.Config.LogoutUri) {
		toa.handleLogout(rw, req)
		return
	}

	cookie, err := req.Cookie(toa.Config.StateCookie.Name)

	if err == nil && strings.HasPrefix(cookie.Value, "Bearer ") {
		token := strings.Trim(strings.TrimPrefix(cookie.Value, "Bearer "), " ")

		var ok = false

		if token != "" {
			isValid, claims, err := validateToken(toa, token)
			if err != nil {
				// TODO: Should we return InternalServerError here?
				log(toa.Config.LogLevel, LogLevelError, "Verifying token: %s", err.Error())
				toa.handleUnauthorized(rw, req)
				return
			}

			ok = isValid

			if ok && claims != nil {
				for _, claimMap := range toa.Config.Headers.MapClaims {
					for claimName, claimValue := range *claims {
						if claimName == claimMap.Claim {
							req.Header.Set(claimMap.Header, fmt.Sprintf("%s", claimValue))
							break
						}
					}
				}
			}
		}

		if !ok {
			http.SetCookie(rw, &http.Cookie{
				Name:    toa.Config.StateCookie.Name,
				Value:   "",
				Path:    toa.Config.StateCookie.Path,
				Expires: time.Now().Add(-24 * time.Hour),
				MaxAge:  -1,
			})

			toa.handleUnauthorized(rw, req)
			return
		}

		// Forward the request
		toa.next.ServeHTTP(rw, req)
		return
	}

	toa.handleUnauthorized(rw, req)
}

func (toa *TraefikOidcAuth) handleCallback(rw http.ResponseWriter, req *http.Request) {
	base64State := req.URL.Query().Get("state")
	if base64State == "" {
		log(toa.Config.LogLevel, LogLevelWarn, "State is missing, redirect to Provider")
		toa.redirectToProvider(rw, req)
		return
	}

	state, err := base64DecodeState(base64State)
	if err != nil {
		log(toa.Config.LogLevel, LogLevelWarn, "State is invalid, redirect to Provider")
		toa.redirectToProvider(rw, req)
		return
	}

	redirectUrl := state.RedirectUrl

	if state.Action == "Login" {
		authCode := req.URL.Query().Get("code")
		if authCode == "" {
			log(toa.Config.LogLevel, LogLevelWarn, "Code is missing, redirect to Provider")
			toa.redirectToProvider(rw, req)
			return
		}

		token, err := exchangeAuthCode(toa, req, authCode, state)
		if err != nil {
			log(toa.Config.LogLevel, LogLevelError, "Exchange Auth Code: %s", err.Error())
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		redactedToken := token
		if len(redactedToken) > 16 {
			redactedToken = redactedToken[0:16] + " *** REDACTED ***"
		}

		_, claims, err := validateToken(toa, token)
		if err != nil {
			log(toa.Config.LogLevel, LogLevelError, "Returned token is not valid: %s", err.Error())
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		log(toa.Config.LogLevel, LogLevelInfo, "Exchange Auth Code completed. Token: %+v", redactedToken)

		if !toa.isAuthorized(claims) {
			http.Error(rw, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}

		// Set the cookie
		http.SetCookie(rw, &http.Cookie{
			Name:     toa.Config.StateCookie.Name,
			Value:    "Bearer " + token,
			Secure:   toa.Config.StateCookie.Secure,
			HttpOnly: toa.Config.StateCookie.HttpOnly,
			Path:     toa.Config.StateCookie.Path,
			SameSite: parseCookieSameSite(toa.Config.StateCookie.SameSite),
		})

		http.SetCookie(rw, &http.Cookie{
			Name:     "CodeVerifier",
			Value:    "",
			Expires:  time.Now().Add(-24 * time.Hour),
			MaxAge:   -1,
			Secure:   true,
			HttpOnly: true,
			Path:     toa.Config.CallbackUri,
			SameSite: http.SameSiteDefaultMode,
		})

		// If we have a static redirect uri, use this one
		if toa.Config.PostLoginRedirectUri != "" {
			redirectUrl = ensureAbsoluteUrl(req, toa.Config.PostLoginRedirectUri)
		}
	} else if state.Action == "Logout" {
		log(toa.Config.LogLevel, LogLevelDebug, "Post logout. Clearing cookie.")

		// Clear the cookie
		http.SetCookie(rw, &http.Cookie{
			Name:    toa.Config.StateCookie.Name,
			Value:   "",
			Path:    toa.Config.StateCookie.Path,
			Expires: time.Now().Add(-24 * time.Hour),
			MaxAge:  -1,
		})
	}

	log(toa.Config.LogLevel, LogLevelInfo, "Redirecting to %s", redirectUrl)

	http.Redirect(rw, req, redirectUrl, http.StatusFound)
}

func (toa *TraefikOidcAuth) handleLogout(rw http.ResponseWriter, req *http.Request) {
	log(toa.Config.LogLevel, LogLevelInfo, "Logging out...")

	// https://openid.net/specs/openid-connect-rpinitiated-1_0.html

	endSessionURL, err := url.Parse(toa.DiscoveryDocument.EndSessionEndpoint)
	if err != nil {
		log(toa.Config.LogLevel, LogLevelError, "Error while parsing the AuthorizationEndpoint: %s", err.Error())
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	callbackUri := ensureAbsoluteUrl(req, toa.Config.CallbackUri)
	redirectUri := ensureAbsoluteUrl(req, toa.Config.PostLogoutRedirectUri)

	if req.URL.Query().Get("redirect_uri") != "" {
		redirectUri = ensureAbsoluteUrl(req, req.URL.Query().Get("redirect_uri"))
	} else if req.URL.Query().Get("post_logout_redirect_uri") != "" {
		redirectUri = ensureAbsoluteUrl(req, req.URL.Query().Get("post_logout_redirect_uri"))
	}

	state := OidcState{
		Action:      "Logout",
		RedirectUrl: redirectUri,
	}

	base64State, err := state.base64Encode()
	if err != nil {
		log(toa.Config.LogLevel, LogLevelError, "Failed to serialize state: %s", err.Error())
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	endSessionURL.RawQuery = url.Values{
		"client_id":                {toa.Config.Provider.ClientId},
		"post_logout_redirect_uri": {callbackUri},
		"state":                    {base64State},
	}.Encode()

	http.Redirect(rw, req, endSessionURL.String(), http.StatusFound)
}

func (toa *TraefikOidcAuth) isAuthorized(claims *jwt.MapClaims) bool {
	authorization := toa.Config.Authorization

	if authorization.AssertClaims != nil && len(authorization.AssertClaims) > 0 {
		parsed := parseJsonRecursively(*claims)

		assertions: for _, assertion := range authorization.AssertClaims {
			value, ok := parsed[assertion.Name]
			if !ok {
				log(toa.Config.LogLevel, LogLevelWarn, "Unauthorized. Unable to find claim %s in token claims.", assertion.Name)
				return false
			}

			switch val := value.(type) {
				// the value is any array
				case []string:
					if assertion.ContainsAny != nil && len(assertion.ContainsAny) > 0 {
						// check whether any of the values of `ContainsAny` is contained in `val`
						for _, inner_val := range val {
							if slices.Contains(assertion.ContainsAny, inner_val) {
								continue assertions
							}
						}
						log(toa.Config.LogLevel, LogLevelWarn, "Unauthorized. Expected claim %s of type array to contain any of [%s].", assertion.Name, strings.Join(assertion.ContainsAny, ", "))
					} else if assertion.Contains == "" || slices.Contains(val, assertion.Contains) {
						continue assertions
					} else {
						log(toa.Config.LogLevel, LogLevelWarn, "Unauthorized. Expected claim %s of type array to contain %s.", assertion.Name, assertion.Contains)
					}
				// the value is of type int or string
				case string:
					if assertion.Values != nil && len(assertion.Values) > 0 {
						// check whether any of the values contains `val`
						if slices.Contains(assertion.Values, val) {
							continue assertions
						}
						log(toa.Config.LogLevel, LogLevelWarn, "Unauthorized. Expected value of claim %s to be one of [%s].", assertion.Name, strings.Join(assertion.Values, ", "))
					} else if assertion.Value == "" || assertion.Value == val {
						continue assertions
					} else {
						log(toa.Config.LogLevel, LogLevelWarn, "Unauthorized. Expected value of claim %s to be %s.", assertion.Name, assertion.Value)
					}
			}

			log(toa.Config.LogLevel, LogLevelInfo, "Available claims are:")
			for key, val := range *claims {
				log(toa.Config.LogLevel, LogLevelInfo, "  %v = %v", key, val)
			}

			return false
		}
	}

	return true
}

func (toa *TraefikOidcAuth) handleUnauthorized(rw http.ResponseWriter, req *http.Request) {
	if toa.Config.LoginUri == "" {
		toa.redirectToProvider(rw, req)
	} else {
		http.Error(rw, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
	}
}

func (toa *TraefikOidcAuth) redirectToProvider(rw http.ResponseWriter, req *http.Request) {
	log(toa.Config.LogLevel, LogLevelInfo, "Redirecting to OIDC provider...")

	host := getFullHost(req)

	originalUrl := fmt.Sprintf("%s%s", host, req.RequestURI)
	redirectUrl := host + toa.Config.CallbackUri

	state := OidcState{
		Action:      "Login",
		RedirectUrl: originalUrl,
	}

	stateBytes, _ := json.Marshal(state)
	stateBase64 := base64.StdEncoding.EncodeToString(stateBytes)

	log(toa.Config.LogLevel, LogLevelDebug, "AuthorizationEndPoint: %s", toa.DiscoveryDocument.AuthorizationEndpoint)

	redirectURL, err := url.Parse(toa.DiscoveryDocument.AuthorizationEndpoint)
	if err != nil {
		log(toa.Config.LogLevel, LogLevelError, "Error while parsing the AuthorizationEndpoint: %s", err.Error())
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	urlValues := url.Values{
		"response_type": {"code"},
		"scope":         {strings.Join(toa.Config.Scopes, " ")},
		"client_id":     {toa.Config.Provider.ClientId},
		"redirect_uri":  {redirectUrl},
		"state":         {stateBase64},
	}

	if toa.Config.Provider.UsePkce {
		codeVerifier, err := randomBytesInHex(32)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		sha2 := sha256.New()
		io.WriteString(sha2, codeVerifier)
		codeChallenge := base64.RawURLEncoding.EncodeToString(sha2.Sum(nil))

		urlValues.Add("code_challenge_method", "S256")
		urlValues.Add("code_challenge", codeChallenge)

		encryptedCodeVerifier, err := encrypt(codeVerifier, toa.Config.Secret)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		// TODO: Make configurable
		http.SetCookie(rw, &http.Cookie{
			Name:     "CodeVerifier",
			Value:    encryptedCodeVerifier,
			Secure:   true,
			HttpOnly: true,
			Path:     toa.Config.CallbackUri,
			SameSite: http.SameSiteDefaultMode,
		})
	}

	redirectURL.RawQuery = urlValues.Encode()

	http.Redirect(rw, req, redirectURL.String(), http.StatusFound)
}
