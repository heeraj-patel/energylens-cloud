package app

import (
	"bytes"
	"errors"
	"fmt"

	"energylens.local/cloud/internal/account"
	"github.com/jackc/pgx/v5/pgxpool"
)

// buildLocalAuth assembles the complete local route and verifier together.
// A disabled deployment never opens key files or installs either component.
func buildLocalAuth(c Config, db *pgxpool.Pool) (account.LocalAuthHTTP, account.Verifier, error) {
	if !c.LocalAuthEnabled {
		return account.LocalAuthHTTP{}, nil, nil
	}
	if db == nil {
		return account.LocalAuthHTTP{}, nil, errors.New("local auth requires database pool")
	}
	files, err := c.localAuthKeyFiles()
	if err != nil {
		return account.LocalAuthHTTP{}, nil, err
	}
	codec, err := account.LoadLocalAccessCodec(files)
	if err != nil {
		return account.LocalAuthHTTP{}, nil, fmt.Errorf("load local auth signing keys: %w", err)
	}
	registerKey, err := account.LoadLocalRateKey(c.LocalAuthRegisterKey)
	if err != nil {
		return account.LocalAuthHTTP{}, nil, fmt.Errorf("load local auth registration rate key: %w", err)
	}
	loginKey, err := account.LoadLocalRateKey(c.LocalAuthLoginKey)
	if err != nil {
		return account.LocalAuthHTTP{}, nil, fmt.Errorf("load local auth login rate key: %w", err)
	}
	recoveryKey, err := account.LoadLocalRateKey(c.LocalAuthRecoveryKey)
	if err != nil {
		return account.LocalAuthHTTP{}, nil, fmt.Errorf("load local auth recovery rate key: %w", err)
	}
	if bytes.Equal(registerKey, loginKey) || bytes.Equal(registerKey, recoveryKey) || bytes.Equal(loginKey, recoveryKey) {
		return account.LocalAuthHTTP{}, nil, errors.New("local auth rate keys must be independent")
	}
	sender, err := c.BuildLocalAuthSender()
	if err != nil || sender == nil {
		return account.LocalAuthHTTP{}, nil, errors.New("local auth mail unavailable")
	}
	source, err := c.LocalAuthSourcePolicy()
	if err != nil {
		return account.LocalAuthHTTP{}, nil, err
	}
	http := account.LocalAuthHTTP{
		Enabled:      true,
		Registration: account.LocalRegistrationService{DB: db, Sender: sender, RateKey: registerKey},
		Login:        account.LocalLoginService{DB: db, Codec: codec, RateKey: loginKey},
		Refresh:      account.LocalRefreshService{DB: db, Codec: codec},
		Recovery:     account.LocalRecoveryService{DB: db, Sender: sender, RateKey: recoveryKey},
		SourcePolicy: source,
	}
	return http, account.LocalAccessVerifier{DB: db, Codec: codec}, nil
}
