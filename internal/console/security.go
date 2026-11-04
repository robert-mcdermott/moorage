package console

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"rsc.io/qr"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// securityView is the page of how someone signs in.
type securityView struct {
	On        bool
	CodesLeft int
	// URI, Key and QR say how to add the account to an authenticator app, while setting it up.
	URI, Key string
	QR       template.HTML
	// NewCodes are recovery codes just made, shown once.
	NewCodes []string
}

// qrSVG draws a QR code as an image, for a page with no scripts. All of it is numbers that it made.
func qrSVG(text string) (template.HTML, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges" aria-hidden="true"><path fill="#000" d="`, code.Size, code.Size)
	for y := range code.Size {
		for x := range code.Size {
			if code.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x, y)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return template.HTML(b.String()), nil // #nosec: only numbers go in
}

func (c *Console) loadSecurity(r *http.Request, view *securityView, userID string) func(context.Context, auth.Principal, *accountView) error {
	return func(ctx context.Context, p auth.Principal, account *accountView) error {
		on, err := c.auth.NeedsSecondFactor(ctx, p.UserID)
		if err != nil {
			return err
		}
		view.On = on
		if on {
			if view.CodesLeft, err = c.db.RecoveryCodesLeft(ctx, p.UserID); err != nil {
				return err
			}
		} else if u, err := c.db.UserByID(ctx, p.UserID); err == nil {
			setup, pending, err := c.auth.CurrentSecondFactorSetup(ctx, u, c.issuerName())
			if err != nil {
				return err
			}
			if pending {
				view.URI, view.Key = setup.URI, setup.Key
				if view.QR, err = qrSVG(setup.URI); err != nil {
					return err
				}
			}
		}
		account.Security = *view
		return nil
	}
}

// issuerName is what an authenticator app calls this server.
func (c *Console) issuerName() string { return "Moorage " + c.cfg.ConsoleHost }

func (c *Console) showSecurity(w http.ResponseWriter, r *http.Request, p auth.Principal, code int, view securityView, failed *failure) {
	c.showAccount(w, r, p, "security", "Two-factor sign-in", "account-security", c.loadSecurity(r, &view, p.UserID), code, accountView{}, failed)
}

func (c *Console) securityPage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	c.showSecurity(w, r, p, http.StatusOK, securityView{}, nil)
}

func (c *Console) startSecondFactor(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	u, err := c.db.UserByID(r.Context(), p.UserID)
	if err != nil {
		c.internal(w, err)
		return
	}
	if _, err := c.auth.BeginSecondFactor(r.Context(), u, c.issuerName()); err != nil {
		c.showSecurity(w, r, p, http.StatusConflict, securityView{}, &failure{Message: sentence(err.Error())})
		return
	}
	http.Redirect(w, r, "/account/security", http.StatusSeeOther)
}

func (c *Console) confirmSecondFactor(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	codes, err := c.auth.ConfirmSecondFactor(r.Context(), p.UserID, r.PostFormValue("code"), c.auth.ClientIP(r))
	var limited *auth.RateLimitedError
	switch {
	case errors.Is(err, auth.ErrSecondFactor):
		c.showSecurity(w, r, p, http.StatusUnprocessableEntity, securityView{}, &failure{Message: "That code is not right. Check that the time on your phone is right, and try the next one."})
		return
	case errors.As(err, &limited):
		c.showSecurity(w, r, p, http.StatusTooManyRequests, securityView{}, &failure{Message: "Too many attempts. Try again in a moment."})
		return
	case errors.Is(err, store.ErrNotFound):
		http.Redirect(w, r, "/account/security", http.StatusSeeOther)
		return
	case err != nil:
		c.internal(w, err)
		return
	}
	c.showSecurity(w, r, p, http.StatusOK, securityView{NewCodes: codes}, nil)
}

func (c *Console) turnOffSecondFactor(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	u, err := c.db.UserByID(r.Context(), p.UserID)
	if err != nil {
		c.internal(w, err)
		return
	}
	// Whoever finds a browser left signed in should not be able to do this.
	if _, err := c.auth.SignIn(r.Context(), u.Email, r.PostFormValue("password"), c.auth.ClientIP(r)); err != nil {
		var limited *auth.RateLimitedError
		if errors.As(err, &limited) {
			c.showSecurity(w, r, p, http.StatusTooManyRequests, securityView{}, &failure{Message: "Too many attempts. Try again in a moment."})
			return
		}
		c.showSecurity(w, r, p, http.StatusUnprocessableEntity, securityView{}, &failure{Message: "That is not your password."})
		return
	}
	if err := c.auth.TurnOffSecondFactor(r.Context(), p.UserID, p.Name, c.auth.ClientIP(r)); err != nil {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, "/account/security?done=two-factor-off", http.StatusSeeOther)
}

// adminSecondFactorOff is for someone who has lost their authenticator app and their recovery codes.
func (c *Console) adminSecondFactorOff(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if err := c.auth.TurnOffSecondFactor(r.Context(), r.PostFormValue("user"), p.Name, c.auth.ClientIP(r)); err != nil {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, "/settings/users?done=two-factor-off", http.StatusSeeOther)
}
