// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package user

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/go-macaron/captcha"
	"github.com/pkg/errors"
	log "unknwon.dev/clog/v2"

	"github.com/NII-DG/gogs/internal/conf"
	"github.com/NII-DG/gogs/internal/context"
	"github.com/NII-DG/gogs/internal/db"
	"github.com/NII-DG/gogs/internal/email"
	"github.com/NII-DG/gogs/internal/form"
	"github.com/NII-DG/gogs/internal/tool"
	"github.com/NII-DG/gogs/internal/utils"
	"github.com/NII-DG/gogs/internal/utils/regex"
)

const (
	LOGIN                    = "user/auth/login"
	TWO_FACTOR               = "user/auth/two_factor"
	TWO_FACTOR_RECOVERY_CODE = "user/auth/two_factor_recovery_code"
	SIGNUP                   = "user/auth/signup"
	ACTIVATE                 = "user/auth/activate"
	FORGOT_PASSWORD          = "user/auth/forgot_passwd"
	RESET_PASSWORD           = "user/auth/reset_passwd"
)

// AutoLogin reads cookie and try to auto-login.
func AutoLogin(c *context.Context) (bool, error) {
	if !db.HasEngine {
		return false, nil
	}

	uname := c.GetCookie(conf.Security.CookieUsername)
	if len(uname) == 0 {
		return false, nil
	}

	isSucceed := false
	defer func() {
		if !isSucceed {
			log.Trace("auto-login cookie cleared: %s", uname)
			c.SetCookie(conf.Security.CookieUsername, "", -1, conf.Server.Subpath)
			c.SetCookie(conf.Security.CookieRememberName, "", -1, conf.Server.Subpath)
			c.SetCookie(conf.Security.LoginStatusCookieName, "", -1, conf.Server.Subpath)
		}
	}()

	u, err := db.GetUserByName(uname)
	if err != nil {
		if !db.IsErrUserNotExist(err) {
			return false, fmt.Errorf("get user by name: %v", err)
		}
		return false, nil
	}

	if val, ok := c.GetSuperSecureCookie(u.Rands+u.Passwd, conf.Security.CookieRememberName); !ok || val != u.Name {
		return false, nil
	}

	isSucceed = true
	_ = c.Session.Set("uid", u.ID)
	_ = c.Session.Set("uname", u.Name)
	c.SetCookie(conf.Session.CSRFCookieName, "", -1, conf.Server.Subpath)
	if conf.Security.EnableLoginStatusCookie {
		c.SetCookie(conf.Security.LoginStatusCookieName, "true", 0, conf.Server.Subpath)
	}
	return true, nil
}

func Login(c *context.Context) {
	c.Title("sign_in")

	// Check auto-login
	isSucceed, err := AutoLogin(c)
	if err != nil {
		c.Error(err, "auto login")
		return
	}

	redirectTo := c.Query("redirect_to")
	if len(redirectTo) > 0 {
		c.SetCookie("redirect_to", redirectTo, 0, conf.Server.Subpath)
	} else {
		redirectTo, _ = url.QueryUnescape(c.GetCookie("redirect_to"))
	}

	if isSucceed {
		if tool.IsSameSiteURLPath(redirectTo) {
			c.Redirect(redirectTo)
		} else {
			c.RedirectSubpath("/")
		}
		c.SetCookie("redirect_to", "", -1, conf.Server.Subpath)
		return
	}

	// Display normal login page
	loginSources, err := db.LoginSources.List(db.ListLoginSourceOpts{OnlyActivated: true})
	if err != nil {
		c.Error(err, "list activated login sources")
		return
	}
	c.Data["LoginSources"] = loginSources
	for i := range loginSources {
		if loginSources[i].IsDefault {
			c.Data["DefaultLoginSource"] = loginSources[i]
			c.Data["login_source"] = loginSources[i].ID
			break
		}
	}
	c.Success(LOGIN)
}

func afterLogin(c *context.Context, u *db.User, remember bool) {
	if remember {
		days := 86400 * conf.Security.LoginRememberDays
		c.SetCookie(conf.Security.CookieUsername, u.Name, days, conf.Server.Subpath, "", conf.Security.CookieSecure, true)
		c.SetSuperSecureCookie(u.Rands+u.Passwd, conf.Security.CookieRememberName, u.Name, days, conf.Server.Subpath, "", conf.Security.CookieSecure, true)
	}

	_ = c.Session.Set("uid", u.ID)
	_ = c.Session.Set("uname", u.Name)
	_ = c.Session.Delete("twoFactorRemember")
	_ = c.Session.Delete("twoFactorUserID")

	// Clear whatever CSRF has right now, force to generate a new one
	c.SetCookie(conf.Session.CSRFCookieName, "", -1, conf.Server.Subpath)
	if conf.Security.EnableLoginStatusCookie {
		c.SetCookie(conf.Security.LoginStatusCookieName, "true", 0, conf.Server.Subpath)
	}

	redirectTo, _ := url.QueryUnescape(c.GetCookie("redirect_to"))
	c.SetCookie("redirect_to", "", -1, conf.Server.Subpath)
	if tool.IsSameSiteURLPath(redirectTo) {
		c.Redirect(redirectTo)
		return
	}

	c.RedirectSubpath("/")
}

func LoginPost(c *context.Context, f form.SignIn) {
	c.Title("sign_in")

	loginSources, err := db.LoginSources.List(db.ListLoginSourceOpts{OnlyActivated: true})
	if err != nil {
		c.Error(err, "list activated login sources")
		return
	}
	c.Data["LoginSources"] = loginSources

	if c.HasError() {
		c.Success(LOGIN)
		return
	}

	u, err := db.Users.Authenticate(f.UserName, f.Password, f.LoginSource)
	if err != nil {
		switch errors.Cause(err).(type) {
		case db.ErrUserNotExist:
			c.FormErr("UserName", "Password")
			c.RenderWithErr(c.Tr("form.username_password_incorrect"), LOGIN, &f)
		case db.ErrLoginSourceMismatch:
			c.FormErr("LoginSource")
			c.RenderWithErr(c.Tr("form.auth_source_mismatch"), LOGIN, &f)

		default:
			c.Error(err, "authenticate user")
		}
		for i := range loginSources {
			if loginSources[i].IsDefault {
				c.Data["DefaultLoginSource"] = loginSources[i]
				break
			}
		}
		return
	}

	if !u.IsEnabledTwoFactor() {
		afterLogin(c, u, f.Remember)
		return
	}

	_ = c.Session.Set("twoFactorRemember", f.Remember)
	_ = c.Session.Set("twoFactorUserID", u.ID)
	c.RedirectSubpath("/user/login/two_factor")
}

func LoginTwoFactor(c *context.Context) {
	_, ok := c.Session.Get("twoFactorUserID").(int64)
	if !ok {
		c.NotFound()
		return
	}

	c.Success(TWO_FACTOR)
}

func LoginTwoFactorPost(c *context.Context) {
	userID, ok := c.Session.Get("twoFactorUserID").(int64)
	if !ok {
		c.NotFound()
		return
	}

	t, err := db.TwoFactors.GetByUserID(userID)
	if err != nil {
		c.Error(err, "get two factor by user ID")
		return
	}

	passcode := c.Query("passcode")
	valid, err := t.ValidateTOTP(passcode)
	if err != nil {
		c.Error(err, "validate TOTP")
		return
	} else if !valid {
		c.Flash.Error(c.Tr("settings.two_factor_invalid_passcode"))
		c.RedirectSubpath("/user/login/two_factor")
		return
	}

	u, err := db.GetUserByID(userID)
	if err != nil {
		c.Error(err, "get user by ID")
		return
	}

	// Prevent same passcode from being reused
	if c.Cache.IsExist(u.TwoFactorCacheKey(passcode)) {
		c.Flash.Error(c.Tr("settings.two_factor_reused_passcode"))
		c.RedirectSubpath("/user/login/two_factor")
		return
	}
	if err = c.Cache.Put(u.TwoFactorCacheKey(passcode), 1, 60); err != nil {
		log.Error("Failed to put cache 'two factor passcode': %v", err)
	}

	afterLogin(c, u, c.Session.Get("twoFactorRemember").(bool))
}

func LoginTwoFactorRecoveryCode(c *context.Context) {
	_, ok := c.Session.Get("twoFactorUserID").(int64)
	if !ok {
		c.NotFound()
		return
	}

	c.Success(TWO_FACTOR_RECOVERY_CODE)
}

func LoginTwoFactorRecoveryCodePost(c *context.Context) {
	userID, ok := c.Session.Get("twoFactorUserID").(int64)
	if !ok {
		c.NotFound()
		return
	}

	if err := db.UseRecoveryCode(userID, c.Query("recovery_code")); err != nil {
		if db.IsTwoFactorRecoveryCodeNotFound(err) {
			c.Flash.Error(c.Tr("auth.login_two_factor_invalid_recovery_code"))
			c.RedirectSubpath("/user/login/two_factor_recovery_code")
		} else {
			c.Error(err, "use recovery code")
		}
		return
	}

	u, err := db.GetUserByID(userID)
	if err != nil {
		c.Error(err, "get user by ID")
		return
	}
	afterLogin(c, u, c.Session.Get("twoFactorRemember").(bool))
}

func SignOut(c *context.Context) {
	_ = c.Session.Flush()
	_ = c.Session.Destory(c.Context)
	c.SetCookie(conf.Security.CookieUsername, "", -1, conf.Server.Subpath)
	c.SetCookie(conf.Security.CookieRememberName, "", -1, conf.Server.Subpath)
	c.SetCookie(conf.Session.CSRFCookieName, "", -1, conf.Server.Subpath)
	c.RedirectSubpath("/")
}

func SignUp(c *context.Context) {
	c.Title("sign_up")

	c.Data["EnableCaptcha"] = conf.Auth.EnableRegistrationCaptcha
	list, err := db.GetAffiliationList()
	if err != nil {
		log.Error("Failed to get affiliation: %v", err)
	}
	c.Data["AffiliationList"] = list

	if conf.Auth.DisableRegistration {
		c.Data["DisableRegistration"] = true
		c.Success(SIGNUP)
		return
	}

	c.Success(SIGNUP)
}

func SignUpPost(c *context.Context, cpt *captcha.Captcha, f form.Register) {
	c.Title("sign_up")

	c.Data["EnableCaptcha"] = conf.Auth.EnableRegistrationCaptcha
	list, err := db.GetAffiliationList()
	if err != nil {
		log.Error("Failed to get affiliation: %v", err)
	}
	c.Data["AffiliationList"] = list

	if conf.Auth.DisableRegistration {
		c.Status(403)
		return
	}

	if c.HasError() {
		c.Success(SIGNUP)
		return
	}

	if conf.Auth.EnableRegistrationCaptcha && !cpt.VerifyReq(c.Req) {
		c.FormErr("Captcha")
		c.RenderWithErr(c.Tr("form.captcha_incorrect"), SIGNUP, &f)
		return
	}

	if f.Password != f.Retype {
		c.FormErr("Password")
		c.RenderWithErr(c.Tr("form.password_not_match"), SIGNUP, &f)
		return
	}
	// check telephone format
	if len(f.Telephone) > 0 && !regex.CheckTelephoneFormat(f.Telephone) {
		c.FormErr("Telephone")
		c.RenderWithErr(c.Tr("form.enterred_invalid_telephone"), SIGNUP, &f)
		return
	}
	// check ORCID URL
	// if PersonalURL is set
	if len( f.PersonalURL ) > 0 {
		orcid_domain := "orcid.org"
		// No Error check  because Checked for URL format at bind time
		parsedURL, _ := url.Parse(f.PersonalURL)
		urlDomain := parsedURL.Hostname()
		if strings.EqualFold( urlDomain, orcid_domain ) {
			value := parsedURL.Path
			if !regex.CheckORCIDFormat(value[1:]) {
				c.FormErr("PersonalUrl")
				c.RenderWithErr(c.Tr("form.enterred_invalid_orcid_url"), SIGNUP, &f)
				return
			}
		}
	}
	// check e-Rad Rearcher Number
	if len(f.ERadResearcherNumber) > 0 && !regex.CheckERadRearcherNumberFormat(f.ERadResearcherNumber) {
		c.FormErr("ERad")
		c.RenderWithErr(c.Tr("form.enterred_invalid_erad"), SIGNUP, &f)
		return
	}

	// Check if spaces are included.
	if utils.ContainsSpace(f.FirstName) || utils.ContainsSpace(f.LastName) {
		c.FormErr("Name")
		c.RenderWithErr(c.Tr("form.must_not_only_space"), SIGNUP, &f)
		return
	}

	// generate User.FullName
	fullName := ""
	if !regex.CheckAlphabet(f.FirstName) || !regex.CheckAlphabet(f.LastName) {
		// japanese user name
		fullName = fmt.Sprintf("%s %s", f.LastName, f.FirstName)
	} else {
		fullName = fmt.Sprintf("%s %s", f.FirstName, f.LastName)
	}

	u := &db.User{
		Name:                 f.UserName,
		Email:                f.Email,
		Telephone:            f.Telephone,
		Passwd:               f.Password,
		FullName:             fullName,
		FirstName:            f.FirstName,
		AliasName:            f.AliasName,
		LastName:             f.LastName,
		ERadResearcherNumber: f.ERadResearcherNumber,
		PersonalURL:          f.PersonalURL,
		AffiliationId:        f.AffiliationId,
		IsActive:             !conf.Auth.RequireEmailConfirmation,
	}
	if err := db.CreateUser(u); err != nil {
		switch {
		case db.IsErrUserAlreadyExist(err):
			c.FormErr("UserName")
			c.RenderWithErr(c.Tr("form.username_been_taken"), SIGNUP, &f)
		case db.IsErrEmailAlreadyUsed(err):
			c.FormErr("Email")
			c.RenderWithErr(c.Tr("form.email_been_used"), SIGNUP, &f)
		case db.IsErrNameNotAllowed(err):
			c.FormErr("UserName")
			c.RenderWithErr(c.Tr("user.form.name_not_allowed", err.(db.ErrNameNotAllowed).Value()), SIGNUP, &f)
		// GIN specific code: case db.IsErrBlockedDomain
		case db.IsErrBlockedDomain(err):
			c.RenderWithErr(c.Tr("form.email_not_allowed"), SIGNUP, &f)
		// RCOS specific code 
		case db.IsErrPasswordInvalid(err):
			c.FormErr("Password")
			c.RenderWithErr( c.Tr("user.form.passward_is_invalid" ), SIGNUP, &f)
		default:
			c.Error(err, "create user")
		}
		return
	}
	log.Trace("Account created: %s", u.Name)

	// temporally implementation
	if strings.HasPrefix(f.UserName, "fa-") {
		u.Type = db.UserFA
		if err := db.UpdateUser(u); err != nil {
			c.Error(err, "update user: UserType")
			return
		}
	}

	// Auto-set admin for the only user.
	if db.CountUsers() == 1 {
		u.IsAdmin = true
		u.IsActive = true
		if err := db.UpdateUser(u); err != nil {
			c.Error(err, "update user")
			return
		}
	}

	// Send confirmation email, no need for social account.
	if conf.Auth.RegisterEmailConfirm && u.ID > 1 {
		email.SendActivateAccountMail(c.Context, db.NewMailerUser(u))
		c.Data["IsSendRegisterMail"] = true
		c.Data["Email"] = u.Email
		c.Data["Hours"] = conf.Auth.ActivateCodeLives / 60
		c.Success(ACTIVATE)

		if err := c.Cache.Put(u.MailResendCacheKey(), 1, 180); err != nil {
			log.Error("Failed to put cache key 'mail resend': %v", err)
		}
		return
	}

	c.RedirectSubpath("/user/login")
}

func Activate(c *context.Context) {
	code := c.Query("code")
	if len(code) == 0 {
		c.Data["IsActivatePage"] = true
		if c.User.IsActive {
			c.NotFound()
			return
		}
		// Resend confirmation email.
		if conf.Auth.RequireEmailConfirmation {
			if c.Cache.IsExist(c.User.MailResendCacheKey()) {
				c.Data["ResendLimited"] = true
			} else {
				c.Data["Hours"] = conf.Auth.ActivateCodeLives / 60
				email.SendActivateAccountMail(c.Context, db.NewMailerUser(c.User))

				if err := c.Cache.Put(c.User.MailResendCacheKey(), 1, 180); err != nil {
					log.Error("Failed to put cache key 'mail resend': %v", err)
				}
			}
		} else {
			c.Data["ServiceNotEnabled"] = true
		}
		c.Success(ACTIVATE)
		return
	}

	// Verify code.
	if user := db.VerifyUserActiveCode(code); user != nil {
		user.IsActive = true
		var err error
		if user.Rands, err = db.GetUserSalt(); err != nil {
			c.Error(err, "get user salt")
			return
		}
		if err := db.UpdateUser(user); err != nil {
			c.Error(err, "update user")
			return
		}

		log.Trace("User activated: %s", user.Name)

		_ = c.Session.Set("uid", user.ID)
		_ = c.Session.Set("uname", user.Name)
		c.RedirectSubpath("/")
		return
	}

	c.Data["IsActivateFailed"] = true
	c.Success(ACTIVATE)
}

func ActivateEmail(c *context.Context) {
	code := c.Query("code")
	emailAddr := c.Query("email")

	// Verify code.
	if email := db.VerifyActiveEmailCode(code, emailAddr); email != nil {
		if err := email.Activate(); err != nil {
			c.Error(err, "activate email")
		}

		log.Trace("Email activated: %s", email.Email)
		c.Flash.Success(c.Tr("settings.add_email_success"))
	}

	c.RedirectSubpath("/user/settings/email")
}

func ForgotPasswd(c *context.Context) {
	c.Title("auth.forgot_password")

	if !conf.Email.Enabled {
		c.Data["IsResetDisable"] = true
		c.Success(FORGOT_PASSWORD)
		return
	}

	c.Data["IsResetRequest"] = true
	c.Success(FORGOT_PASSWORD)
}

func ForgotPasswdPost(c *context.Context) {
	c.Title("auth.forgot_password")

	if !conf.Email.Enabled {
		c.Status(403)
		return
	}
	c.Data["IsResetRequest"] = true

	emailAddr := c.Query("email")
	c.Data["Email"] = emailAddr

	u, err := db.GetUserByEmail(emailAddr)
	if err != nil {
		if db.IsErrUserNotExist(err) {
			c.Data["Hours"] = conf.Auth.ActivateCodeLives / 60
			c.Data["IsResetSent"] = true
			c.Success(FORGOT_PASSWORD)
			return
		}

		c.Error(err, "get user by email")
		return
	}

	if !u.IsLocal() {
		c.FormErr("Email")
		c.RenderWithErr(c.Tr("auth.non_local_account"), FORGOT_PASSWORD, nil)
		return
	}

	if c.Cache.IsExist(u.MailResendCacheKey()) {
		c.Data["ResendLimited"] = true
		c.Success(FORGOT_PASSWORD)
		return
	}

	email.SendResetPasswordMail(c.Context, db.NewMailerUser(u))
	if err = c.Cache.Put(u.MailResendCacheKey(), 1, 180); err != nil {
		log.Error("Failed to put cache key 'mail resend': %v", err)
	}

	c.Data["Hours"] = conf.Auth.ActivateCodeLives / 60
	c.Data["IsResetSent"] = true
	c.Success(FORGOT_PASSWORD)
}

func ResetPasswd(c *context.Context) {
	c.Title("auth.reset_password")

	code := c.Query("code")
	if len(code) == 0 {
		c.NotFound()
		return
	}
	c.Data["Code"] = code
	c.Data["IsResetForm"] = true
	c.Success(RESET_PASSWORD)
}

func ResetPasswdPost(c *context.Context) {
	c.Title("auth.reset_password")

	code := c.Query("code")
	if len(code) == 0 {
		c.NotFound()
		return
	}
	c.Data["Code"] = code

	if u := db.VerifyUserActiveCode(code); u != nil {
		// Validate password length.
		passwd := c.Query("password")
		if len(passwd) < 6 {
			c.Data["IsResetForm"] = true
			c.Data["Err_Password"] = true
			c.RenderWithErr(c.Tr("auth.password_too_short"), RESET_PASSWORD, nil)
			return
		}

		u.Passwd = passwd
		var err error
		if u.Rands, err = db.GetUserSalt(); err != nil {
			c.Error(err, "get user salt")
			return
		}
		if u.Salt, err = db.GetUserSalt(); err != nil {
			c.Error(err, "get user salt")
			return
		}
		u.EncodePassword()
		if err := db.UpdateUser(u); err != nil {
			c.Error(err, "update user")
			return
		}

		log.Trace("User password reset: %s", u.Name)
		c.RedirectSubpath("/user/login")
		return
	}

	c.Data["IsResetFailed"] = true
	c.Success(RESET_PASSWORD)
}
