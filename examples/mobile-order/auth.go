package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	cookieName      = "mobile_order_session"
	browserLifetime = 14 * 24 * time.Hour
	hashPrefix      = "pbkdf2-sha256"
)

// hashIterations is the PBKDF2 iteration count new hashes get. It is a variable only so the
// tests can lower it: a check reads the count from the hash, so hashes made with fewer
// iterations take the same path through login.
var hashIterations = 600_000

// randomToken returns n random bytes in base64url, which fits BarPay's patterns for keys and
// state ([A-Za-z0-9_.:-]).
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// randomID returns an ID of letters and digits only, for member IDs and order IDs.
func randomID(prefix string) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return prefix + string(b)
}

func hashPassword(password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, hashIterations, 32)
	if err != nil {
		panic(err)
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("%s$%d$%s$%s", hashPrefix, hashIterations, enc.EncodeToString(salt), enc.EncodeToString(key))
}

func isPasswordHash(h string) bool {
	parts := strings.Split(h, "$")
	return len(parts) == 4 && parts[0] == hashPrefix
}

func checkPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != hashPrefix {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// browser returns the browser session of the request, starting one when there is none. It
// returns the cookie value and a copy of the session; change it with app.updateBrowser.
func (a *App) browser(w http.ResponseWriter, r *http.Request) (string, Browser) {
	if c, err := r.Cookie(cookieName); err == nil {
		var b Browser
		found := false
		a.store.view(func(d *Data) {
			if v := d.Browsers[c.Value]; v != nil && time.Now().Before(v.ExpiresAt) {
				b, found = *v, true
			}
		})
		if found {
			return c.Value, b
		}
	}
	id := randomToken(32)
	b := Browser{CSRF: randomToken(32), ExpiresAt: time.Now().Add(browserLifetime)}
	a.store.update(func(d *Data) error {
		cp := b
		d.Browsers[id] = &cp
		return nil
	})
	a.setCookie(w, id)
	return id, b
}

func (a *App) setCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    id,
		Path:     "/",
		MaxAge:   int(browserLifetime.Seconds()),
		HttpOnly: true,
		Secure:   a.cfg.secureCookies(),
		// Lax keeps the cookie on the top-level return from PayPal, which is how the return
		// page finds the browser session that asked for the link.
		SameSite: http.SameSiteLaxMode,
	})
}

// renewBrowser replaces the browser session's ID after a login, so an ID known before it is
// worth nothing after it.
func (a *App) renewBrowser(w http.ResponseWriter, oldID string, fn func(b *Browser)) {
	id := randomToken(32)
	a.store.update(func(d *Data) error {
		b := d.Browsers[oldID]
		if b == nil {
			b = &Browser{}
		}
		delete(d.Browsers, oldID)
		b.CSRF = randomToken(32)
		b.ExpiresAt = time.Now().Add(browserLifetime)
		fn(b)
		d.Browsers[id] = b
		return nil
	})
	a.setCookie(w, id)
}

func (a *App) updateBrowser(id string, fn func(b *Browser)) {
	a.store.update(func(d *Data) error {
		if b := d.Browsers[id]; b != nil {
			fn(b)
		}
		return nil
	})
}

// checkCSRF says whether a form came from a page this app drew for the same browser session.
func checkCSRF(r *http.Request, b Browser) bool {
	got := r.PostFormValue("csrf")
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(b.CSRF)) == 1
}

// staffLogin checks a staff ID and password against the config file. The ID it accepts is the
// one sent to BarPay as BarPay-Staff-Id.
func (a *App) staffLogin(id, password string) bool {
	hash, ok := a.cfg.Staff[id]
	if !ok {
		// Spend the same time as a known ID, so the answer does not tell which IDs exist.
		checkPassword(a.dummyHash, password)
		return false
	}
	return checkPassword(hash, password)
}
