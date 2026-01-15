package headervalidator

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"

	"regexp"
	"strconv"
	"time"

	"github.com/ONDC-Official/automation-beckn-plugins/workbench-main/internal/apiservice"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"golang.org/x/crypto/blake2b"
)


type HeaderValidator struct {
	RegistryURL string
	KeyManager definition.KeyManager
}

func NewHeaderValidator(registryURL string, keyManager definition.KeyManager) *HeaderValidator {
	return &HeaderValidator{
		RegistryURL: registryURL,
		KeyManager: keyManager,
	}
}

func (hv *HeaderValidator) ValidateHeaders(ctx context.Context, request *http.Request, payloadEnv apiservice.PayloadEnvelope, raw apiservice.PayloadRaw) error {
	return nil
}


func verifyAuthorizationHeader(authHeader, payload, publicKey string) (bool, string) {
    // Parse authorization header
    _, created, expires, signature, err := parseAuthHeader(authHeader)
    if err != nil {
        return false, "error parsing auth header"
    }

    // Validate timestamp
    currentTimestamp := time.Now().Unix()
    createdInt, _ := strconv.ParseInt(created, 10, 64)
    expiresInt, _ := strconv.ParseInt(expires, 10, 64)

    if createdInt > currentTimestamp || currentTimestamp > expiresInt {
        return false, "Authorization header expired or not yet valid"
    }

    // Hash the exact payload string
    hash := blake2b.Sum512([]byte(payload))
    digest := base64.StdEncoding.EncodeToString(hash[:])

    // Create signing string
    signingString := fmt.Sprintf("(created): %s\n(expires): %s\ndigest: BLAKE-512=%s", 
        created, expires, digest)

    // Verify signature
    publicKeyBytes, _ := base64.StdEncoding.DecodeString(publicKey)
    signatureBytes, _ := base64.StdEncoding.DecodeString(signature)

    isValid := ed25519.Verify(publicKeyBytes, []byte(signingString), signatureBytes)
    if isValid {
        return true, ""
    }
    return false, "Authorization header verification failed"
}

func parseAuthHeader(authHeader string) (string, string, string, string, error) {
	signatureRegex := regexp.MustCompile(`keyId=\"(.+?)\".+?created=\"(.+?)\".+?expires=\"(.+?)\".+?signature=\"(.+?)\"`)
	groups := signatureRegex.FindAllStringSubmatch(authHeader, -1)
	if len(groups) > 0 && len(groups[0]) > 4 {
		return groups[0][1], groups[0][2], groups[0][3], groups[0][4], nil
	}
	fmt.Println("Error parsing auth header. Please make sure that the auh headers passed as command line argument is valid")
	return "", "", "", "", errors.New("error parsing auth header")
}