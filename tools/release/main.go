// Command release creates the signing key for Collie releases and signs
// (and checks) a release's SHA256SUMS.
//
//	go run ./tools/release keygen -out FILE            new key pair: the private key goes to FILE,
//	                                                   the public key is added to internal/update/release.pub
//	go run ./tools/release sign -tag v0.3.0 SUMS       writes SUMS.sig (private key in $COLLIE_SIGNING_KEY)
//	go run ./tools/release verify -tag v0.3.0 SUMS     checks SUMS.sig against release.pub
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/tattsun/collie/internal/update"
)

const pubFile = "internal/update/release.pub"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: release keygen -out FILE | sign -tag TAG SHA256SUMS | verify -tag TAG SHA256SUMS")
	os.Exit(2)
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "file to write the private key to (must not exist)")
	_ = fs.Parse(args)
	if *out == "" {
		return errors.New("-out is required")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv.Seed()))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	pf, err := os.OpenFile(pubFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("%w (run from the repository root)", err)
	}
	defer pf.Close()
	if _, err := fmt.Fprintln(pf, base64.StdEncoding.EncodeToString(pub)); err != nil {
		return err
	}
	fmt.Printf("private key: %s\npublic key added to %s\n", *out, pubFile)
	return nil
}

func sumsArgs(name string, args []string) (tag, sums string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	t := fs.String("tag", "", "release tag, e.g. v0.3.0")
	_ = fs.Parse(args)
	if *t == "" || fs.NArg() != 1 {
		usage()
	}
	return *t, fs.Arg(0)
}

func sign(args []string) error {
	tag, path := sumsArgs("sign", args)
	key, err := update.ParsePrivateKey(os.Getenv("COLLIE_SIGNING_KEY"))
	if err != nil {
		return fmt.Errorf("COLLIE_SIGNING_KEY: %w", err)
	}
	sums, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".sig", update.Sign(key, tag, sums), 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s for %s\n", path, tag)
	return nil
}

func verify(args []string) error {
	tag, path := sumsArgs("verify", args)
	sums, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(path + ".sig")
	if err != nil {
		return err
	}
	if err := update.VerifySums(update.Keys(), tag, sums, sig); err != nil {
		return fmt.Errorf("%w (is %s the public half of COLLIE_SIGNING_KEY?)", err, pubFile)
	}
	fmt.Printf("%s.sig is valid for %s\n", path, tag)
	return nil
}
