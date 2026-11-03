package enroll

import (
	"regexp"
	"strings"
	"testing"
)

func TestTokenFormat(t *testing.T) {
	pattern := regexp.MustCompile(`^ENR(-[0-9A-HJKMNP-TV-Z]{1,4})+$`)

	seen := map[string]bool{}
	for range 100 {
		token, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if !pattern.MatchString(token) {
			t.Fatalf("unexpected format: %s", token)
		}
		if seen[token] {
			t.Fatalf("a repeated token: %s", token)
		}
		seen[token] = true

		// 26 characters of base32 hold 128 bits
		if letters := len(strings.ReplaceAll(strings.TrimPrefix(token, "ENR-"), "-", "")); letters != 26 {
			t.Fatalf("%d characters in %s", letters, token)
		}
	}
}

func TestHash(t *testing.T) {
	const token = "ENR-7Q2K-XM4P-9TLD"

	hash := Hash(token)

	if strings.Contains(hash, "7Q2K") {
		t.Fatal("the hash contains the token")
	}
	if len(hash) != 64 {
		t.Fatalf("length %d", len(hash))
	}
	if Hash(token) != hash {
		t.Fatal("the hash must be stable")
	}
	if Hash("enr-7q2k-xm4p-9tld\n") != hash {
		t.Fatal("a copied token with a line break or in lower case must still match")
	}
	if Hash("ENR-7Q2K-XM4P-9TLE") == hash {
		t.Fatal("another token must not match")
	}
}
