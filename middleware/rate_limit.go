package middleware

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"api-gateway/client"
	ua "github.com/mileusna/useragent"
)

type rateLimitData struct {
	Seconds int
	Nonce   string
}

func generateNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "fallback-nonce"
	}
	return base64.StdEncoding.EncodeToString(b)
}

// RateLimit checks the Rate Limiter Service before passing to next.
// Skips the rate limiter if the request host is not in the routes map.
// If the request is rate limited, it responds with 429 and stops.
func RateLimit(rl *client.RateLimiterClient, routes map[string]string, ipEncryptionKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, exactMatch := routes[r.Host]
		_, wwwMatch := routes[strings.TrimPrefix(r.Host, "www.")]
		_, keyMatch := routes[resolveServiceIdentifier(r)]
		if !exactMatch && !wwwMatch && !keyMatch {
			slog.Warn("host not in routes, rejecting", "host", r.Host)
			http.Error(w, "unknown host", http.StatusBadGateway)
			return
		}

		clientIP := r.Header.Get("X-Client-ID")
		if clientIP == "" {
			clientIP = remoteIP(r)
		}
		clientIP = encryptIP(clientIP, ipEncryptionKey)

		serviceIdentifier := resolveServiceIdentifier(r)
		meta := buildRequestMeta(r)

		result, err := rl.IsAllowed(serviceIdentifier, clientIP, r.URL.Path, r.Method, meta)
		if err != nil {
			slog.Error("rate limiter error",
				"error", err,
				"serviceIdentifier", serviceIdentifier,
				"httpMethod", r.Method,
			)
		}

		if result.Forbidden {
			slog.Warn("request forbidden: path not configured in rate limiter",
				"serviceIdentifier", serviceIdentifier,
				"path", r.URL.Path,
				"httpMethod", r.Method,
			)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		if !result.Allowed {
			slog.Warn("request rate limited",
				"serviceIdentifier", serviceIdentifier,
				"httpMethod", r.Method,
			)
			retryAfter := result.RetryAfterSecs
			if retryAfter <= 0 {
				retryAfter = result.ResetAfterSeconds
			}
			nonce := generateNonce()
			w.Header().Set("RateLimit-Limit", fmt.Sprintf("%d", result.Limit))
			w.Header().Set("RateLimit-Remaining", "0")
			w.Header().Set("RateLimit-Reset", fmt.Sprintf("%d", result.ResetAfterSeconds))
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
			w.Header().Set("Cache-Control", "no-store")
			// Override the global CSP to allow the nonce-tagged inline style and script
			// on this error page. 'unsafe-inline' is NOT used; only the nonce is trusted.
			w.Header().Set("Content-Security-Policy", fmt.Sprintf(
				"default-src 'none'; style-src 'nonce-%s'; script-src 'nonce-%s'", nonce, nonce,
			))
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			rateLimitTmpl.Execute(w, rateLimitData{Seconds: retryAfter, Nonce: nonce}) //nolint:errcheck
			return
		}

		if result.Limit > 0 {
			w.Header().Set("RateLimit-Limit", fmt.Sprintf("%d", result.Limit))
			w.Header().Set("RateLimit-Remaining", fmt.Sprintf("%d", result.Remaining))
			w.Header().Set("RateLimit-Reset", fmt.Sprintf("%d", result.ResetAfterSeconds))
		}

		next.ServeHTTP(w, r)
	})
}

// resolveServiceIdentifier returns a single value to identify the service:
//   - Subdomain from Host header (e.g. "personal-website.example.com" → "personal-website")
//   - Port from Host header if request is to an IP (e.g. "5.78.139.110:8085" → "8085")
func resolveServiceIdentifier(r *http.Request) string {
	host := r.Host
	port := ""
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		port = host[idx+1:]
		host = host[:idx]
	}
	if net.ParseIP(host) != nil {
		return port
	}
	host = strings.TrimPrefix(host, "www.")
	parts := strings.Split(host, ".")
	if len(parts) >= 2 {
		return parts[0]
	}
	return ""
}

// fallbackKey is a random per-process key used when IP_ENCRYPTION_KEY is not set.
// Rate limiting still works but encrypted IPs cannot be decrypted across restarts.
var fallbackKey = func() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}()

// encryptIP encrypts the IP with AES-256-GCM using a deterministic nonce so the
// same IP always produces the same ciphertext — required for rate limit counting.
// The result is base64url-encoded and safe to use as a rate limiter client ID.
func encryptIP(ip, key string) string {
	if key == "" {
		key = fallbackKey
	}
	k := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return ip
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ip
	}
	// Deterministic nonce: HMAC(key, ip) truncated to nonce size.
	// Same IP + same key → same nonce → same ciphertext (needed for rate limiting).
	mac := hmac.New(sha256.New, k[:])
	mac.Write([]byte(ip))
	nonce := mac.Sum(nil)[:gcm.NonceSize()]

	ciphertext := gcm.Seal(nil, nonce, []byte(ip), nil)
	return base64.URLEncoding.EncodeToString(append(nonce, ciphertext...))
}


// remoteIP extracts the client IP, respecting X-Forwarded-For.
func remoteIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.SplitN(ip, ",", 2)[0]
	}
	if idx := strings.LastIndex(r.RemoteAddr, ":"); idx != -1 {
		return r.RemoteAddr[:idx]
	}
	return r.RemoteAddr
}

// buildRequestMeta parses the request and returns structured metadata for the rate limiter.
func buildRequestMeta(r *http.Request) client.RequestMeta {
	parsed := ua.Parse(r.Header.Get("User-Agent"))
	return client.RequestMeta{
		DeviceType:  deviceType(parsed),
		IsBot:       parsed.Bot,
		BotName:     botName(parsed),
		Browser:     parsed.Name,
		OS:          parsed.OS,
		RequestSize: r.ContentLength,
	}
}

func deviceType(parsed ua.UserAgent) string {
	if parsed.Bot {
		return "bot"
	}
	if parsed.Mobile {
		return "mobile"
	}
	if parsed.Tablet {
		return "tablet"
	}
	if parsed.Desktop {
		return "desktop"
	}
	return "unknown"
}

func botName(parsed ua.UserAgent) string {
	if parsed.Bot {
		return parsed.Name
	}
	return ""
}

var rateLimitTmpl = template.Must(template.New("ratelimit").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>429 — Too Many Requests</title>
  <style nonce="{{.Nonce}}">
    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 16px;
      background: linear-gradient(135deg, #0f0c29, #302b63, #24243e);
      color: #fff;
    }

    .card {
      background: rgba(255, 255, 255, 0.06);
      backdrop-filter: blur(16px);
      -webkit-backdrop-filter: blur(16px);
      border: 1px solid rgba(255, 255, 255, 0.12);
      border-radius: 24px;
      padding: 48px 40px 40px;
      text-align: center;
      width: 100%;
      max-width: 460px;
      box-shadow: 0 24px 64px rgba(0, 0, 0, 0.4);
    }

    .badge {
      display: inline-block;
      background: rgba(249, 115, 22, 0.18);
      color: #fb923c;
      border: 1px solid rgba(249, 115, 22, 0.35);
      border-radius: 999px;
      font-size: 12px;
      font-weight: 600;
      letter-spacing: 0.08em;
      text-transform: uppercase;
      padding: 4px 14px;
      margin-bottom: 24px;
    }

    h1 {
      font-size: clamp(22px, 5vw, 28px);
      font-weight: 700;
      letter-spacing: -0.02em;
      margin-bottom: 12px;
      line-height: 1.2;
    }

    .subtitle {
      color: rgba(255,255,255,0.5);
      font-size: 15px;
      line-height: 1.6;
      margin-bottom: 36px;
    }

    /* Timer ring */
    .timer-wrap {
      position: relative;
      width: 120px;
      height: 120px;
      margin: 0 auto 32px;
    }
    .timer-wrap svg {
      transform: rotate(-90deg);
      width: 100%;
      height: 100%;
    }
    .ring-bg {
      fill: none;
      stroke: rgba(255,255,255,0.08);
      stroke-width: 6;
    }
    .ring-fg {
      fill: none;
      stroke: url(#grad);
      stroke-width: 6;
      stroke-linecap: round;
      stroke-dasharray: 326.73;
      stroke-dashoffset: 0;
    }
    .timer-inner {
      position: absolute;
      inset: 0;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      gap: 2px;
    }
    .timer-seconds {
      font-size: 32px;
      font-weight: 800;
      line-height: 1;
      background: linear-gradient(135deg, #fb923c, #f43f5e);
      -webkit-background-clip: text;
      -webkit-text-fill-color: transparent;
      background-clip: text;
    }
    .timer-unit {
      font-size: 11px;
      font-weight: 500;
      color: rgba(255,255,255,0.35);
      text-transform: uppercase;
      letter-spacing: 0.1em;
    }

    /* Ready state */
    .timer-seconds.done { font-size: 26px; }

    /* Refresh button */
    .btn {
      display: inline-flex;
      align-items: center;
      gap: 8px;
      background: linear-gradient(135deg, #f97316, #ef4444);
      color: #fff;
      font-size: 15px;
      font-weight: 600;
      padding: 14px 32px;
      border-radius: 12px;
      border: none;
      cursor: pointer;
      text-decoration: none;
      box-shadow: 0 4px 20px rgba(249, 115, 22, 0.4);
      transition: transform 0.15s ease, box-shadow 0.15s ease, opacity 0.15s ease;
    }
    .btn:disabled {
      opacity: 0.35;
      cursor: not-allowed;
      box-shadow: none;
      transform: none;
    }
    .btn:not(:disabled):hover { transform: translateY(-2px); box-shadow: 0 8px 28px rgba(249, 115, 22, 0.55); }
    .btn:not(:disabled):active { transform: translateY(0); }

    .btn svg { width: 16px; height: 16px; flex-shrink: 0; }

    .hint {
      margin-top: 16px;
      font-size: 13px;
      color: rgba(255,255,255,0.25);
    }

    @media (max-width: 480px) {
      .card { padding: 36px 24px 32px; }
    }
  </style>
</head>
<body>
  <div class="card">
    <div class="badge">429 &mdash; Rate Limited</div>
    <h1>Too many requests</h1>
    <p class="subtitle">You&rsquo;ve hit the request limit for this service.<br>Please wait for the timer to reset before trying again.</p>

    <div class="timer-wrap">
      <svg viewBox="0 0 110 110" xmlns="http://www.w3.org/2000/svg">
        <defs>
          <linearGradient id="grad" x1="0%" y1="0%" x2="100%" y2="100%">
            <stop offset="0%" stop-color="#fb923c"/>
            <stop offset="100%" stop-color="#f43f5e"/>
          </linearGradient>
        </defs>
        <circle class="ring-bg" cx="55" cy="55" r="52"/>
        <circle class="ring-fg" id="ring" cx="55" cy="55" r="52"/>
      </svg>
      <div class="timer-inner">
        <span class="timer-seconds" id="countdown">{{.Seconds}}</span>
        <span class="timer-unit" id="unit">sec</span>
      </div>
    </div>

    <button class="btn" id="refreshBtn" disabled>
      <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
        <path d="M21 2v6h-6"/><path d="M3 12a9 9 0 0 1 15-6.7L21 8"/><path d="M3 22v-6h6"/><path d="M21 12a9 9 0 0 1-15 6.7L3 16"/>
      </svg>
      Try again
    </button>

    <p class="hint" id="hint">Button will be enabled when ready</p>
  </div>

  <script nonce="{{.Nonce}}">
    (function () {
      var total = {{.Seconds}};
      var circumference = 2 * Math.PI * 52;
      var ring = document.getElementById('ring');
      var label = document.getElementById('countdown');
      var unit = document.getElementById('unit');
      var btn = document.getElementById('refreshBtn');
      var hint = document.getElementById('hint');

      btn.addEventListener('click', function () { window.location.reload(); });

      ring.style.strokeDasharray = circumference;

      function markReady() {
        label.textContent = '✓';
        label.classList.add('done');
        unit.textContent = 'ready';
        ring.style.strokeDashoffset = circumference;
        btn.removeAttribute('disabled');
        hint.style.display = 'none';
      }

      if (total <= 0) {
        ring.style.strokeDashoffset = circumference;
        markReady();
        return;
      }

      ring.style.strokeDashoffset = 0;
      var startTime = performance.now();

      function tick() {
        var remaining = total - (performance.now() - startTime) / 1000;
        if (remaining <= 0) {
          markReady();
          return;
        }
        label.textContent = Math.ceil(remaining);
        ring.style.strokeDashoffset = circumference * (1 - remaining / total);
        requestAnimationFrame(tick);
      }

      requestAnimationFrame(tick);
    })();
  </script>
</body>
</html>
`))
