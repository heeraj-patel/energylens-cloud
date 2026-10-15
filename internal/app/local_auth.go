package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"energylens.local/cloud/internal/account"
	"github.com/jackc/pgx/v5/pgxpool"
)

type localAuthMaterial struct {
	codec       account.LocalAccessCodec
	registerKey []byte
	loginKey    []byte
	recoveryKey []byte
}

// CheckLocalAuthKeys validates the exact file-backed material startup will
// use, without opening a database or sending mail. During rotation, provide
// the previous key ID and its pinned public key path to catch accidental
// removal or replacement under the same ID.
func CheckLocalAuthKeys(c Config, previousID, previousPublicPath string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !c.LocalAuthEnabled {
		return errors.New("local auth is disabled")
	}
	if (previousID == "") != (previousPublicPath == "") {
		return errors.New("previous key ID and public key path must be provided together")
	}
	material, err := loadLocalAuthMaterial(c)
	if err != nil {
		return err
	}
	if previousID != "" {
		files, err := c.localAuthKeyFiles()
		if err != nil {
			return err
		}
		candidatePath, ok := files.PublicPaths[previousID]
		if !ok {
			return errors.New("previous local auth public key is missing")
		}
		candidateInfo, err := os.Stat(candidatePath)
		if err != nil {
			return err
		}
		previousInfo, err := os.Stat(previousPublicPath)
		if err != nil {
			return err
		}
		if os.SameFile(candidateInfo, previousInfo) {
			return errors.New("previous local auth public key reference must be a separate file")
		}
		previous, err := account.LoadLocalPublicKey(previousPublicPath)
		if err != nil {
			return fmt.Errorf("load previous local auth public key: %w", err)
		}
		retained, ok := material.codec.Keys[previousID]
		if !ok || !bytes.Equal(retained, previous) {
			return errors.New("previous local auth public key is missing or changed")
		}
	}
	return nil
}

func loadLocalAuthMaterial(c Config) (localAuthMaterial, error) {
	files, err := c.localAuthKeyFiles()
	if err != nil {
		return localAuthMaterial{}, err
	}
	codec, err := account.LoadLocalAccessCodec(files)
	if err != nil {
		return localAuthMaterial{}, fmt.Errorf("load local auth signing keys: %w", err)
	}
	registerKey, err := account.LoadLocalRateKey(c.LocalAuthRegisterKey)
	if err != nil {
		return localAuthMaterial{}, fmt.Errorf("load local auth registration rate key: %w", err)
	}
	loginKey, err := account.LoadLocalRateKey(c.LocalAuthLoginKey)
	if err != nil {
		return localAuthMaterial{}, fmt.Errorf("load local auth login rate key: %w", err)
	}
	var recoveryKey []byte
	if c.RuntimeEnvironment != "local" {
		recoveryKey, err = account.LoadLocalRateKey(c.LocalAuthRecoveryKey)
		if err != nil {
			return localAuthMaterial{}, fmt.Errorf("load local auth recovery rate key: %w", err)
		}
	}
	if bytes.Equal(registerKey, loginKey) || recoveryKey != nil && (bytes.Equal(registerKey, recoveryKey) || bytes.Equal(loginKey, recoveryKey)) {
		return localAuthMaterial{}, errors.New("local auth rate keys must be independent")
	}
	return localAuthMaterial{codec: codec, registerKey: registerKey, loginKey: loginKey, recoveryKey: recoveryKey}, nil
}

// buildLocalAuth assembles the complete local route and verifier together.
// A disabled deployment never opens key files or installs either component.
func buildLocalAuth(c Config, db *pgxpool.Pool) (account.LocalAuthHTTP, account.Verifier, error) {
	if !c.LocalAuthEnabled {
		return account.LocalAuthHTTP{}, nil, nil
	}
	if db == nil {
		return account.LocalAuthHTTP{}, nil, errors.New("local auth requires database pool")
	}
	material, err := loadLocalAuthMaterial(c)
	if err != nil {
		return account.LocalAuthHTTP{}, nil, err
	}
	var sender *account.ResendLocalSender
	if c.RuntimeEnvironment != "local" {
		configured, err := c.BuildLocalAuthSender()
		if err != nil || configured == nil {
			return account.LocalAuthHTTP{}, nil, errors.New("local auth mail unavailable")
		}
		sender = configured
	}
	source, err := c.LocalAuthSourcePolicy()
	if err != nil {
		return account.LocalAuthHTTP{}, nil, err
	}
	profile := account.LocalEmailProfile
	if c.RuntimeEnvironment == "local" {
		profile = account.LocalUsernameProfile
	}
	http := account.LocalAuthHTTP{
		Enabled:      true,
		Registration: account.LocalRegistrationService{DB: db, Sender: sender, RateKey: material.registerKey, Profile: profile},
		Login:        account.LocalLoginService{DB: db, Codec: material.codec, RateKey: material.loginKey},
		Refresh:      account.LocalRefreshService{DB: db, Codec: material.codec},
		Recovery:     account.LocalRecoveryService{DB: db, Sender: sender, RateKey: material.recoveryKey},
		SourcePolicy: source,
	}
	return http, account.LocalAccessVerifier{DB: db, Codec: material.codec}, nil
}
