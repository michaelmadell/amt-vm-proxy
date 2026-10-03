// Copied from device-management-toolkit/go-wsman-messages pkg/wsman/client/hash.go (Apache-2.0).
package main

import (
	"crypto/md5"
	"fmt"
	"io"
	"log"
)

type AuthChallenge struct {
	Username   string
	Password   string
	Realm      string
	CSRFToken  string
	Domain     string
	Nonce      string
	Opaque     string
	Stale      string
	Algorithm  string
	Qop        string
	CNonce     string
	NonceCount int
}

func hashWithMD5(data string) string {
	md5Hash := md5.New()

	_, err := io.WriteString(md5Hash, data)
	if err != nil {
		log.Println("failed to write string to md5 hash")
	}

	return fmt.Sprintf("%x", md5Hash.Sum(nil))
}
func hashWithHash(secret, data string) string {
	return hashWithMD5(fmt.Sprintf("%s:%s", secret, data))
}
func (c *AuthChallenge) HashCredentials() string {
	return hashWithMD5(fmt.Sprintf("%s:%s:%s", c.Username, c.Realm, c.Password))
}
func (c *AuthChallenge) hashURI(method, uri string) string {
	return hashWithMD5(fmt.Sprintf("%s:%s", method, uri))
}
func (c *AuthChallenge) GetFormattedNonceData(nonceData string) string {
	nonceData = fmt.Sprintf("%s:%08x:%s:%s", nonceData, c.NonceCount, c.CNonce, c.Qop)

	return nonceData
}
func (c *AuthChallenge) ComputeDigestHash(method, uri, nonceData string) string {
	hashedCredentials := c.HashCredentials()
	hashedURI := c.hashURI(method, uri)
	response := hashWithHash(hashedCredentials, fmt.Sprintf("%s:%s", nonceData, hashedURI))

	return response
}
