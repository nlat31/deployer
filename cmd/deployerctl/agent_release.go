package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"deployer/internal/selfupdate"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
)

func agentRelease(command string, args []string) error {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	keyPath := f.String("key", "", "base64 Ed25519 private key file")
	pubPath := f.String("public", "", "public key output")
	binary := f.String("binary", "", "agent binary")
	version := f.String("version", "", "release version")
	arch := f.String("arch", "amd64", "architecture")
	out := f.String("out", "manifest.json", "signed manifest output")
	if e := f.Parse(args); e != nil {
		return e
	}
	if *keyPath == "" {
		return fmt.Errorf("--key required")
	}
	if command == "update-keygen" {
		if *pubPath == "" {
			return fmt.Errorf("--public required")
		}
		pub, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return e
		}
		// Never replace an existing signing key.
		for path, data := range map[string][]byte{*keyPath: key, *pubPath: pub} {
			fd, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
			_, e = fd.WriteString(base64.StdEncoding.EncodeToString(data) + "\n")
			ce := fd.Close()
			if e != nil {
				return e
			}
			if ce != nil {
				return ce
			}
		}
		return nil
	}
	key, e := selfupdate.ReadKey(*keyPath)
	if e != nil {
		return e
	}
	if len(key) != ed25519.PrivateKeySize {
		return fmt.Errorf("invalid private key")
	}
	digest, size, e := selfupdate.Digest(*binary)
	if e != nil {
		return e
	}
	m := selfupdate.Manifest{Version: *version, OS: "linux", Arch: *arch, Size: size, Digest: digest, Compatibility: selfupdate.Compatibility, Format: selfupdate.Format}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, m.Payload()))
	if e = m.Verify(ed25519.PrivateKey(key).Public().(ed25519.PublicKey)); e != nil {
		return e
	}
	return selfupdate.Save(*out, m)
}
