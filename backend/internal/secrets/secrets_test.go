package secrets

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func testBox(t *testing.T, keyByte byte) *Box {
	t.Helper()
	b, err := New(bytes.Repeat([]byte{keyByte}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSealOpenRoundTrip(t *testing.T) {
	b := testBox(t, 1)
	for _, plaintext := range []string{"", "s3cret", strings.Repeat("long value ", 1000)} {
		sealed, err := b.Seal([]byte(plaintext), []byte("app=1 key=TOKEN"))
		if err != nil {
			t.Fatal(err)
		}
		if plaintext != "" && bytes.Contains(sealed, []byte(plaintext)) {
			t.Errorf("sealed value contains the plaintext %q", plaintext)
		}
		got, err := b.Open(sealed, []byte("app=1 key=TOKEN"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if string(got) != plaintext {
			t.Errorf("Open = %q, want %q", got, plaintext)
		}
	}
}

func TestSealUsesFreshNonces(t *testing.T) {
	b := testBox(t, 1)
	first, _ := b.Seal([]byte("same"), nil)
	second, _ := b.Seal([]byte("same"), nil)
	if bytes.Equal(first, second) {
		t.Error("sealing the same value twice gave identical bytes; want a fresh nonce each time")
	}
}

func TestOpenRejects(t *testing.T) {
	b := testBox(t, 1)
	ad := []byte("app=1 key=TOKEN")
	sealed, err := b.Seal([]byte("s3cret"), ad)
	if err != nil {
		t.Fatal(err)
	}
	flipped := func(i int) []byte {
		c := bytes.Clone(sealed)
		c[i] ^= 0x01
		return c
	}

	tests := []struct {
		name   string
		box    *Box
		sealed []byte
		ad     []byte
	}{
		// The case associated data exists for: a value copied into another
		// app's (or another key's) row.
		{name: "other associated data", box: b, sealed: sealed, ad: []byte("app=2 key=TOKEN")},
		{name: "wrong key", box: testBox(t, 2), sealed: sealed, ad: ad},
		{name: "altered ciphertext", box: b, sealed: flipped(len(sealed) - 20), ad: ad},
		{name: "altered tag", box: b, sealed: flipped(len(sealed) - 1), ad: ad},
		{name: "altered nonce", box: b, sealed: flipped(1), ad: ad},
		{name: "unknown format", box: b, sealed: flipped(0), ad: ad},
		{name: "truncated", box: b, sealed: sealed[:10], ad: ad},
		{name: "empty", box: b, sealed: nil, ad: ad},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.box.Open(tt.sealed, tt.ad)
			if !errors.Is(err, ErrDecrypt) {
				t.Errorf("Open = %q, %v; want ErrDecrypt", got, err)
			}
		})
	}
}

func TestParseKey(t *testing.T) {
	valid := strings.Repeat("ab", KeySize)
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "valid", in: valid},
		{name: "empty", in: "", wantErr: true},
		{name: "too short", in: valid[:62], wantErr: true},
		{name: "too long", in: valid + "ab", wantErr: true},
		{name: "not hex", in: strings.Repeat("zz", KeySize), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := ParseKey(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseKey(%q) succeeded, want error", tt.in)
				}
				if tt.in != "" && strings.Contains(err.Error(), tt.in) {
					t.Errorf("error %q repeats the key", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseKey: %v", err)
			}
			if len(key) != KeySize {
				t.Errorf("key is %d bytes, want %d", len(key), KeySize)
			}
		})
	}
}

func TestNewRejectsWrongKeySize(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := New(make([]byte, n)); err == nil {
			t.Errorf("New with a %d-byte key succeeded, want error", n)
		}
	}
}
