// Copyright 2020 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package db

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
	"github.com/pkg/errors"
	gormbulk "github.com/t-tiger/gorm-bulk-insert"
	log "unknwon.dev/clog/v2"

	"github.com/NII-DG/gogs/internal/cryptoutil"
	"github.com/NII-DG/gogs/internal/errutil"
	"github.com/NII-DG/gogs/internal/strutil"
)

// TwoFactorsStore is the persistent interface for 2FA.
//
// NOTE: All methods are sorted in alphabetical order.
type TwoFactorsStore interface {
	// Create creates a new 2FA token and recovery codes for given user.
	// The "key" is used to encrypt and later decrypt given "secret",
	// which should be configured in site-level and change of the "key"
	// will break all existing 2FA tokens.
	Create(userID int64, key, secret string) error
	// GetByUserID returns the 2FA token of given user.
	// It returns ErrTwoFactorNotFound when not found.
	GetByUserID(userID int64) (*TwoFactor, error)
	// IsUserEnabled returns true if the user has enabled 2FA.
	IsUserEnabled(userID int64) bool
}

var TwoFactors TwoFactorsStore

// NOTE: This is a GORM create hook.
func (t *TwoFactor) BeforeCreate() {
	t.CreatedUnix = gorm.NowFunc().Unix()
}

// NOTE: This is a GORM query hook.
func (t *TwoFactor) AfterFind() {
	t.Created = time.Unix(t.CreatedUnix, 0).Local()
}

var _ TwoFactorsStore = (*twoFactors)(nil)

type twoFactors struct {
	*gorm.DB
}

func (db *twoFactors) Create(userID int64, key, secret string) error {
	encrypted, err := cryptoutil.AESGCMEncrypt(cryptoutil.MD5Bytes(key), []byte(secret))
	if err != nil {
		return errors.Wrap(err, "encrypt secret")
	}
	tf := &TwoFactor{
		UserID: userID,
		Secret: base64.StdEncoding.EncodeToString(encrypted),
	}

	recoveryCodes, err := generateRecoveryCodes(userID, 10)
	if err != nil {
		return errors.Wrap(err, "generate recovery codes")
	}

	records := make([]interface{}, 0, len(recoveryCodes))
	for _, code := range recoveryCodes {
		records = append(records, code)
	}

	return db.Transaction(func(tx *gorm.DB) error {
		err := tx.Create(tf).Error
		if err != nil {
			return err
		}

		return gormbulk.BulkInsert(tx, records, 3000)
	})
}

var _ errutil.NotFound = (*ErrTwoFactorNotFound)(nil)

type ErrTwoFactorNotFound struct {
	args errutil.Args
}

func IsErrTwoFactorNotFound(err error) bool {
	_, ok := err.(ErrTwoFactorNotFound)
	return ok
}

func (err ErrTwoFactorNotFound) Error() string {
	return fmt.Sprintf("2FA does not found: %v", err.args)
}

func (ErrTwoFactorNotFound) NotFound() bool {
	return true
}

func (db *twoFactors) GetByUserID(userID int64) (*TwoFactor, error) {
	tf := new(TwoFactor)
	err := db.Where("user_id = ?", userID).First(tf).Error
	if err != nil {
		if gorm.IsRecordNotFoundError(err) {
			return nil, ErrTwoFactorNotFound{args: errutil.Args{"userID": userID}}
		}
		return nil, err
	}
	return tf, nil
}

func (db *twoFactors) IsUserEnabled(userID int64) bool {
	var count int64
	err := db.Model(new(TwoFactor)).Where("user_id = ?", userID).Count(&count).Error
	if err != nil {
		log.Error("Failed to count two factors [user_id: %d]: %v", userID, err)
	}
	return count > 0
}

// generateRecoveryCodes generates N number of recovery codes for 2FA.
func generateRecoveryCodes(userID int64, n int) ([]*TwoFactorRecoveryCode, error) {
	recoveryCodes := make([]*TwoFactorRecoveryCode, n)
	for i := 0; i < n; i++ {
		code, err := strutil.RandomChars(10)
		if err != nil {
			return nil, errors.Wrap(err, "generate random characters")
		}
		recoveryCodes[i] = &TwoFactorRecoveryCode{
			UserID: userID,
			Code:   strings.ToLower(code[:5] + "-" + code[5:]),
		}
	}
	return recoveryCodes, nil
}
