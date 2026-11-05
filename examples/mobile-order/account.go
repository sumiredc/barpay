package main

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The account: the member's name, the rows to the visit history and the PayPal page, and log out.
// A guest sees the rows a member has, unavailable.

func (a *App) account(w http.ResponseWriter, r *http.Request) {
	id, b := a.browser(w, r)
	if b.MemberID == "" && !b.Guest {
		redirect(w, r, "/")
		return
	}
	v := a.baseView(id, b, "Account")
	data := struct {
		Initial string
		// PayPal is what paypalStatus says, or "" when BarPay could not be read.
		PayPal string
	}{}
	if v.Member != nil {
		r0, _ := utf8.DecodeRuneInString(v.Member.Name)
		data.Initial = string(unicode.ToUpper(r0))
		ctx, cancel := a.ctx(r)
		defer cancel()
		if m, err := a.barpay.getMember(ctx, b.MemberID); err == nil {
			data.PayPal = paypalStatus(m)
		}
	}
	v.Data = data
	a.render(w, http.StatusOK, "account", v)
}

// saveName changes the member's name, under the same rule as sign-up. A guest has no name to
// change.
func (a *App) saveName(w http.ResponseWriter, r *http.Request) {
	id, b, ok := a.postForm(w, r)
	if !ok {
		return
	}
	if b.MemberID == "" {
		redirect(w, r, "/account")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if !validName(name) {
		a.alert(id, "Enter your name (up to 60 characters)")
		redirect(w, r, "/account")
		return
	}
	a.store.update(func(d *Data) error {
		if m := d.Members[b.MemberID]; m != nil {
			m.Name = name
		}
		return nil
	})
	a.toast(id, "Name saved")
	redirect(w, r, "/account")
}
