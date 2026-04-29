package internal

import "fmt"

type AccountVerifyResult struct {
	Email     string `json:"email"`
	Provider  string `json:"provider"`
	SMTPOK    bool   `json:"smtp_ok,omitempty"`
	IMAPOK    bool   `json:"imap_ok,omitempty"`
	SMTPError string `json:"smtp_error,omitempty"`
	IMAPError string `json:"imap_error,omitempty"`
}

func VerifySMTPIMAPAccount(account Account, smtpVerifier SMTPAccountVerifier, imapVerifier IMAPAccountVerifier) (*AccountVerifyResult, error) {
	result := &AccountVerifyResult{
		Email:    account.Email,
		Provider: account.Provider,
	}

	if account.Provider != AccountProviderSMTPIMAP {
		return result, fmt.Errorf("account %s is provider %s, expected %s", account.Email, account.Provider, AccountProviderSMTPIMAP)
	}
	if smtpVerifier == nil {
		smtpVerifier = NewSMTPTransport(nil)
	}
	if imapVerifier == nil {
		imapVerifier = NewIMAPTransport(nil)
	}

	if err := smtpVerifier.VerifyAccount(account); err != nil {
		result.SMTPError = err.Error()
	} else {
		result.SMTPOK = true
	}

	if err := imapVerifier.VerifyAccount(account); err != nil {
		result.IMAPError = err.Error()
	} else {
		result.IMAPOK = true
	}

	if !result.SMTPOK || !result.IMAPOK {
		return result, fmt.Errorf("SMTP/IMAP verification failed for %s", account.Email)
	}
	return result, nil
}
