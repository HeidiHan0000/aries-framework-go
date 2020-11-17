/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package webkms

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/hyperledger/aries-framework-go/pkg/common/log"
	"github.com/hyperledger/aries-framework-go/pkg/kms"
)

const (
	createKeystoreEndpoint = "{serverEndpoint}/kms/keystores"

	// ContentType is remoteKMS http content-type.
	ContentType = "application/json"

	// LocationHeader is remoteKMS http header set by the key server (usually to identify a keystore or key url).
	LocationHeader = "Location"
)

var logger = log.New("aries-framework/kms/webkms")

type createKeystoreReq struct {
	Controller         string `json:"controller,omitempty"`
	OperationalVaultID string `json:"pperationalvaultid,omitempty"`
}

type createKeyReq struct {
	KeyType string `json:"keytype,omitempty"`
}

type exportKeyResp struct {
	KeyBytes string `json:"keyid,omitempty"`
}

type marshalFunc func(interface{}) ([]byte, error)

type unmarshalFunc func([]byte, interface{}) error

// RemoteKMS implementation of kms.KeyManager api.
type RemoteKMS struct {
	httpClient     *http.Client
	keystoreURL    string
	marshalFunc    marshalFunc
	unmarshalFunc  unmarshalFunc
	addHeadersOpts *headersOpts
}

// CreateKeyStore calls the key server's create keystore REST function and returns the resulting keystoreURL value.
// Arguments of this function are described below:
//   - httpClient used to POST the request
//   - keyserverURL representing the key server url
//	 - marshaller the marshal function used for marshaling content in the client. Usually: `json.Marshal`
//   - headersOpt optional function setting any necessary http headers for key server authorization
// Returns:
//  - keystore URL (if successful)
//  - error (if error encountered)
func CreateKeyStore(httpClient *http.Client, keyserverURL, controller, vaultID string, marshaller marshalFunc,
	headersOpts ...HeadersOpt) (string, error) {
	destination := strings.ReplaceAll(createKeystoreEndpoint, "{serverEndpoint}", keyserverURL)
	httpReqJSON := &createKeystoreReq{
		Controller: controller,
	}

	if vaultID != "" {
		httpReqJSON.OperationalVaultID = vaultID
	}

	mReq, err := marshaller(httpReqJSON)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Create keystore request [%s, %w]", destination, err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, destination, bytes.NewBuffer(mReq))
	if err != nil {
		return "", fmt.Errorf("build request for Create keystore error: %w", err)
	}

	httpReq.Header.Set("Content-Type", ContentType)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("posting Create keystore failed [%s, %w]", destination, err)
	}

	// handle response
	defer closeResponseBody(resp.Body, logger, "CreateKeyStore")

	keystoreURL := resp.Header.Get(LocationHeader)

	return keystoreURL, nil
}

// New creates a new remoteKMS instance using http client connecting to keystoreURL.
func New(keystoreURL string, client *http.Client, headersOpts ...HeadersOpt) *RemoteKMS {
	hOpts := NewOpt()

	for _, opt := range headersOpts {
		opt(hOpts)
	}

	return &RemoteKMS{
		httpClient:     client,
		keystoreURL:    keystoreURL,
		marshalFunc:    json.Marshal,
		unmarshalFunc:  json.Unmarshal,
		addHeadersOpts: hOpts,
	}
}

func (r *RemoteKMS) postHTTPRequest(destination string, mReq []byte) (*http.Response, error) {
	return r.doHTTPRequest(http.MethodPost, destination, mReq)
}

func (r *RemoteKMS) getHTTPRequest(destination string) (*http.Response, error) {
	return r.doHTTPRequest(http.MethodGet, destination, nil)
}

func (r *RemoteKMS) doHTTPRequest(method, destination string, mReq []byte) (*http.Response, error) {
	httpReq, err := http.NewRequest(method, destination, bytes.NewBuffer(mReq))
	if err != nil {
		return nil, fmt.Errorf("build request error: %w", err)
	}

	httpReq.Header.Set("Content-Type", ContentType)

	if r.addHeadersOpts.headersFunc != nil {
		httpHeaders, err := r.addHeadersOpts.headersFunc(httpReq)
		if err != nil {
			return nil, fmt.Errorf("add optional request headers error: %w", err)
		}

		if httpHeaders != nil {
			httpReq.Header = httpHeaders.Clone()
		}
	}

	return r.httpClient.Do(httpReq)
}

// Create a new key/keyset/key handle for the type kt remotely
// Returns:
//  - KeyID raw ID of the handle
//  - handle instance representing a remote keystore URL including KeyID
//  - error if failure
func (r *RemoteKMS) Create(kt kms.KeyType) (string, interface{}, error) {
	destination := r.keystoreURL + "/keys"
	httpReqJSON := &createKeyReq{
		KeyType: string(kt),
	}

	marshaledReq, err := r.marshalFunc(httpReqJSON)
	if err != nil {
		return "", nil, fmt.Errorf("failed to marshal Create key request [%s, %w]", destination, err)
	}

	resp, err := r.postHTTPRequest(destination, marshaledReq)
	if err != nil {
		return "", nil, fmt.Errorf("posting Create key failed [%s, %w]", destination, err)
	}

	// handle response
	defer closeResponseBody(resp.Body, logger, "Create")

	kidURL := resp.Header.Get(LocationHeader)
	kid := kidURL[strings.LastIndex(kidURL, "/")+1:]

	return kid, kidURL, nil
}

// Get key handle for the given KeyID remotely
// Returns:
//  - handle instance representing a remote keystore URL including KeyID
//  - error if failure
func (r *RemoteKMS) Get(keyID string) (interface{}, error) {
	return r.buildKIDURL(keyID), nil
}

func (r *RemoteKMS) buildKIDURL(keyID string) string {
	return r.keystoreURL + "/keys/" + keyID
}

// Rotate remotely a key referenced by KeyID and return a new handle of a keyset including old key and
// new key with type kt. It also returns the updated KeyID as the first return value
// Returns:
//  - new KeyID
//  - handle instance (to private key)
//  - error if failure
func (r *RemoteKMS) Rotate(kt kms.KeyType, keyID string) (string, interface{}, error) {
	return "", nil, errors.New("function Rotate is not implemented in remoteKMS")
}

// ExportPubKeyBytes will remotely fetch a key referenced by id then gets its public key in raw bytes and returns it.
// The key must be an asymmetric key.
// Returns:
//  - marshalled public key []byte
//  - error if it fails to export the public key bytes
func (r *RemoteKMS) ExportPubKeyBytes(keyID string) ([]byte, error) {
	keyURL := r.buildKIDURL(keyID)

	destination := keyURL + "/export"

	resp, err := r.getHTTPRequest(destination)
	if err != nil {
		return nil, fmt.Errorf("posting ExportPubKeyBytes key failed [%s, %w]", destination, err)
	}

	// handle response
	defer closeResponseBody(resp.Body, logger, "ExportPubKeyBytes")

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read key response for ExportPubKeyBytes failed [%s, %w]", destination, err)
	}

	httpResp := &exportKeyResp{}

	err = r.unmarshalFunc(respBody, httpResp)
	if err != nil {
		return nil, fmt.Errorf("unmarshal key for ExportPubKeyBytes failed [%s, %w]", destination, err)
	}

	keyBytes, err := base64.URLEncoding.DecodeString(httpResp.KeyBytes)
	if err != nil {
		return nil, err
	}

	return keyBytes, nil
}

// CreateAndExportPubKeyBytes will remotely create a key of type kt and export its public key in raw bytes and returns
// it. The key must be an asymmetric key.
// Returns:
//  - KeyID of the new handle created.
//  - marshalled public key []byte
//  - error if it fails to export the public key bytes
func (r *RemoteKMS) CreateAndExportPubKeyBytes(kt kms.KeyType) (string, []byte, error) {
	kid, _, err := r.Create(kt)
	if err != nil {
		return "", nil, err
	}

	pubKey, err := r.ExportPubKeyBytes(kid)
	if err != nil {
		return "", nil, err
	}

	return kid, pubKey, nil
}

// PubKeyBytesToHandle is not implemented in remoteKMS.
func (r *RemoteKMS) PubKeyBytesToHandle(pubKey []byte, kt kms.KeyType) (interface{}, error) {
	return nil, errors.New("function PubKeyBytesToHandle is not implemented in remoteKMS")
}

// ImportPrivateKey will import privKey into the KMS storage for the given KeyType then returns the new key id and
// the newly persisted Handle.
// 'privKey' possible types are: *ecdsa.PrivateKey and ed25519.PrivateKey
// 'kt' possible types are signing key types only (ECDSA keys or Ed25519)
// 'opts' allows setting the keysetID of the imported key using WithKeyID() option. If the ID is already used,
// then an error is returned.
// Returns:
//  - KeyID of the handle
//  - handle instance (to private key)
//  - error if import failure (key empty, invalid, doesn't match KeyType, unsupported KeyType or storing key failed)
func (r *RemoteKMS) ImportPrivateKey(privKey interface{}, kt kms.KeyType,
	opts ...kms.PrivateKeyOpts) (string, interface{}, error) {
	return "", nil, errors.New("function ImportPrivateKey is not implemented in remoteKMS")
}

// closeResponseBody closes the response body.
//nolint: interfacer // don't want to add test stretcher logger here
func closeResponseBody(respBody io.Closer, logger log.Logger, action string) {
	err := respBody.Close()
	if err != nil {
		logger.Errorf("Failed to close response body for '%s' REST call: %s", action, err.Error())
	}
}
