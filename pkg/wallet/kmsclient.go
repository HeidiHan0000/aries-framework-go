/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package wallet

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bluele/gcache"
	"github.com/google/tink/go/subtle/random"
	"github.com/google/uuid"

	"github.com/hyperledger/aries-framework-go/pkg/crypto"
	"github.com/hyperledger/aries-framework-go/pkg/kms"
	"github.com/hyperledger/aries-framework-go/pkg/kms/localkms"
	"github.com/hyperledger/aries-framework-go/pkg/kms/webkms"
	"github.com/hyperledger/aries-framework-go/pkg/secretlock"
	"github.com/hyperledger/aries-framework-go/pkg/secretlock/local"
	"github.com/hyperledger/aries-framework-go/pkg/secretlock/local/masterlock/hkdf"
	"github.com/hyperledger/aries-framework-go/spi/storage"
)

const (
	// LocalKeyURIPrefix for locally stored keys.
	localKeyURIPrefix = "local-lock://"

	// default cache expiry time.
	defaultCacheExpiry = 10 * time.Minute

	// number of sections in verification method.
	vmSectionCount = 2
)

// errors.
// nolint: gochecknoglobals
var (
	// ErrAlreadyUnlocked error when key manager is already created for a given user.
	ErrAlreadyUnlocked = errors.New("profile already unlocked")

	// WalletLocked when key manager operation is attempted without unlocking wallet.
	ErrWalletLocked = errors.New("wallet locked")
)

// walletKMSInstance is key manager store singleton - access only via keyManager()
//nolint:gochecknoglobals
var (
	walletKMSInstance *walletKeyManager
	kmsStoreOnce      sync.Once
)

func keyManager() *walletKeyManager {
	kmsStoreOnce.Do(func() {
		walletKMSInstance = &walletKeyManager{
			gstore: gcache.New(0).Build(),
		}
	})

	return walletKMSInstance
}

// walletKeyManager manages key manager instances in cache.
// underlying gcache is threasafe, no need of locks.
type walletKeyManager struct {
	gstore gcache.Cache
}

func (k *walletKeyManager) createKeyManager(profileInfo *profile,
	storeProvider storage.Provider, opts *unlockOpts) (string, error) {
	if profileInfo.MasterLockCipher == "" && profileInfo.KeyServerURL == "" {
		return "", fmt.Errorf("invalid wallet profile")
	}

	// get user from cache if token already exists
	token, _ := k.getKeyMangerToken(profileInfo.User) //nolint: errcheck
	if token != "" {
		return "", ErrAlreadyUnlocked
	}

	var err error

	var keyManager kms.KeyManager

	// create key manager
	if profileInfo.MasterLockCipher != "" {
		// local kms
		keyManager, err = createLocalKeyManager(profileInfo.User, opts.passphrase,
			profileInfo.MasterLockCipher, opts.secretLockSvc, storeProvider)
		if err != nil {
			return "", fmt.Errorf("failed to create local key manager: %w", err)
		}
	} else {
		// remote kms
		keyManager = createRemoteKeyManager(opts.authToken, profileInfo.KeyServerURL)
	}

	// generate token
	token = uuid.New().String()

	// save key manager
	err = k.saveKeyManger(profileInfo.User, token, keyManager, opts.tokenExpiry)
	if err != nil {
		return "", fmt.Errorf("failed to persist local key manager: %w", err)
	}

	return token, nil
}

// TODO refresh expiry on each access.
func (k *walletKeyManager) saveKeyManger(user, key string, manager kms.KeyManager, expiration time.Duration) error {
	if expiration == 0 {
		expiration = defaultCacheExpiry
	}

	err := k.gstore.SetWithExpire(user, key, expiration)
	if err != nil {
		return err
	}

	return k.gstore.SetWithExpire(key, manager, expiration)
}

func (k *walletKeyManager) getKeyManger(key string) (kms.KeyManager, error) {
	val, err := k.gstore.Get(key)
	if err != nil {
		return nil, err
	}

	return val.(kms.KeyManager), nil
}

func (k *walletKeyManager) getKeyMangerToken(user string) (string, error) {
	val, err := k.gstore.Get(user)
	if err != nil {
		return "", err
	}

	return val.(string), nil
}

func (k *walletKeyManager) removeKeyManager(user string) bool {
	token, _ := k.getKeyMangerToken(user) //nolint: errcheck
	if token != "" {
		return k.gstore.Remove(token) && k.gstore.Remove(user)
	}

	return false
}

// createMasterLock creates master lock from secret lock service provided.
func createMasterLock(secretLockSvc secretlock.Service) (string, error) {
	masterKeyContent := random.GetRandomBytes(uint32(32)) //nolint: gomnd

	masterLockEnc, err := secretLockSvc.Encrypt(localKeyURIPrefix, &secretlock.EncryptRequest{
		Plaintext: string(masterKeyContent),
	})
	if err != nil {
		return "", fmt.Errorf("failed to create master lock from secret lock service provided: %w", err)
	}

	return masterLockEnc.Ciphertext, nil
}

type kmsProvider struct {
	storageProvider storage.Provider
	secretLock      secretlock.Service
}

func (k kmsProvider) StorageProvider() storage.Provider {
	return k.storageProvider
}

func (k kmsProvider) SecretLock() secretlock.Service {
	return k.secretLock
}

// createLocalKeyManager creates and returns local KMS instance.
func createLocalKeyManager(user, passphrase, masterLockCipher string,
	masterLocker secretlock.Service, storeProvider storage.Provider) (*localkms.LocalKMS, error) {
	var err error
	if passphrase != "" {
		masterLocker, err = getDefaultSecretLock(passphrase)
		if err != nil {
			return nil, err
		}
	}

	secretLockSvc, err := local.NewService(bytes.NewBufferString(masterLockCipher), masterLocker)
	if err != nil {
		return nil, err
	}

	return localkms.New(localKeyURIPrefix+user, &kmsProvider{
		storageProvider: storeProvider,
		secretLock:      secretLockSvc,
	})
}

// getDefaultSecretLock returns hkdf secret lock service from passphrase.
func getDefaultSecretLock(passphrase string) (secretlock.Service, error) {
	return hkdf.NewMasterLock(passphrase, sha256.New, nil)
}

// createLocalKeyManager creates and returns remote KMS instance.
func createRemoteKeyManager(auth, keyServerURL string) *webkms.RemoteKMS {
	return webkms.New(keyServerURL, http.DefaultClient, webkms.WithHeaders(func(req *http.Request) (*http.Header, error) {
		req.Header.Set("authorization", fmt.Sprintf("Bearer %s", auth))

		return &req.Header, nil
	}))
}

type kmsSigner struct {
	keyHandle interface{}
	crypto    crypto.Crypto
	multiMsg  bool
}

func newKMSSigner(authToken string, c crypto.Crypto, opts *ProofOptions) (*kmsSigner, error) {
	keyManager, err := keyManager().getKeyManger(authToken)
	if err != nil {
		if errors.Is(err, gcache.KeyNotFoundError) {
			return nil, ErrWalletLocked
		}

		return nil, fmt.Errorf("failed to get key manager: %w", err)
	}

	vmSplit := strings.Split(opts.VerificationMethod, "#")

	if len(vmSplit) != vmSectionCount {
		return nil, errors.New("invalid verification method format")
	}

	keyHandler, err := keyManager.Get(vmSplit[vmSectionCount-1])
	if err != nil {
		return nil, err
	}

	return &kmsSigner{keyHandle: keyHandler, crypto: c, multiMsg: opts.ProofType == BbsBlsSignature2020}, nil
}

func (s *kmsSigner) textToLines(txt string) [][]byte {
	lines := strings.Split(txt, "\n")
	linesBytes := make([][]byte, 0, len(lines))

	for i := range lines {
		if strings.TrimSpace(lines[i]) != "" {
			linesBytes = append(linesBytes, []byte(lines[i]))
		}
	}

	return linesBytes
}

func (s *kmsSigner) Sign(data []byte) ([]byte, error) {
	if s.multiMsg {
		return s.crypto.SignMulti(s.textToLines(string(data)), s.keyHandle)
	}

	v, err := s.crypto.Sign(data, s.keyHandle)
	if err != nil {
		return nil, err
	}

	return v, nil
}