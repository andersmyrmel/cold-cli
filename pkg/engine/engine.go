// Package engine exposes the cold-cli engine to hosted wrappers without
// exposing the repository's internal package layout.
package engine

import (
	"database/sql"

	"github.com/anders/cold-cli/internal"
)

type Account = internal.Account
type AccountVerifyResult = internal.AccountVerifyResult
type AddSMTPIMAPAccountOpts = internal.AddSMTPIMAPAccountOpts
type AddSMTPIMAPAccountResult = internal.AddSMTPIMAPAccountResult
type RemoveAccountResult = internal.PauseAccountResult
type SecretResolver = internal.SecretResolver
type SecretResolverFunc = internal.SecretResolverFunc
type SendSMTPTestEmailOpts = internal.SendSMTPTestEmailOpts
type SendSMTPTestEmailResult = internal.SendSMTPTestEmailResult
type Store = internal.Store
type TickConfig = internal.TickConfig
type TickResult = internal.TickResult
type UpdateAccountOpts = internal.UpdateAccountOpts
type UpdateSMTPIMAPAccountOpts = internal.UpdateSMTPIMAPAccountOpts

const (
	AccountProviderGWS      = internal.AccountProviderGWS
	AccountProviderSMTPIMAP = internal.AccountProviderSMTPIMAP
)

func OpenStore() (*Store, error) {
	return internal.OpenStore()
}

func AddSMTPIMAPAccount(db *sql.DB, opts AddSMTPIMAPAccountOpts) (*AddSMTPIMAPAccountResult, error) {
	return internal.AddSMTPIMAPAccount(db, opts)
}

func UpdateSMTPIMAPAccount(db *sql.DB, email string, opts UpdateSMTPIMAPAccountOpts) (*AddSMTPIMAPAccountResult, error) {
	return internal.UpdateSMTPIMAPAccount(db, email, opts)
}

func GetAccountByEmail(db *sql.DB, email string) (Account, error) {
	return internal.GetAccountByEmail(db, email)
}

func RemoveAccount(db *sql.DB, email string) (*RemoveAccountResult, error) {
	return internal.RemoveAccount(db, email)
}

func UpdateAccount(db *sql.DB, email string, opts UpdateAccountOpts) error {
	return internal.UpdateAccount(db, email, opts)
}

func VerifySMTPIMAPAccount(account Account, resolver SecretResolver) (*AccountVerifyResult, error) {
	return internal.VerifySMTPIMAPAccount(
		account,
		internal.NewSMTPTransport(resolver),
		internal.NewIMAPTransport(resolver),
	)
}

func SendSMTPTestEmail(account Account, opts SendSMTPTestEmailOpts, resolver SecretResolver) (*SendSMTPTestEmailResult, error) {
	return internal.SendSMTPTestEmail(account, opts, resolver)
}

func Tick(cfg TickConfig) (*TickResult, error) {
	return internal.Tick(cfg)
}
