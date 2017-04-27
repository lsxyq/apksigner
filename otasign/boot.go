package otasign

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"

	"playground/android"
	"playground/log"
)

// See https://source.android.com/security/verifiedboot/verified-boot

type AlgorithmID struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type AuthAttr struct {
	Target string
	Length int
}

type BootSigASN1 struct {
	Version int
	Cert asn1.RawValue
	AlgorithmIDs AlgorithmID
	AuthAttrs AuthAttr
	Signature []byte
}

type BootSig struct {
	BootSigASN1
	target string
	signed []byte
}

func ParseBootSig(b []byte) (*BootSig, error) {
	sig := BootSig{}

	rest, err := asn1.Unmarshal(b, &(sig.BootSigASN1))
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, errors.New("boot signature has excess bytes")
	}

	if len(sig.Signature) < 0 {
		return nil, errors.New("missing actual signature")
	}

	if sig.Version != 1 {
		return nil, errors.New(fmt.Sprintf("unsupported boot signature version %d", sig.Version))
	}

	return &sig, nil
}

func NewBootSig(target string, b []byte) *BootSig {
	sig := BootSig{}
	sig.Version = 1

	sig.target = target
	sig.signed = make([]byte, len(b))
	copy(sig.signed, b)

	return &sig
}

func (sig *BootSig) Verify(b []byte) error {
	// if there was no cert, something is fishy
	cert, err := x509.ParseCertificate(sig.Cert.FullBytes)
	if err != nil {
		return errors.New("error parsing signer certificate")
	}

	// we only know how to RSA for now -- TODO: add more
	if cert.PublicKeyAlgorithm != x509.RSA {
		return errors.New("unsupported public key algorithm")
	}

	// select the hash to use from object ID; we only know how to SHA2-256 & -512 for now; TODO: add more
	var hash crypto.Hash
	pub := cert.PublicKey.(*rsa.PublicKey)
	aid := sig.AlgorithmIDs.Algorithm
	RSA_SHA256 := asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	RSA_SHA512 := asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 13}
	if aid.Equal(RSA_SHA256) {
		hash = crypto.SHA256
	} else if aid.Equal(RSA_SHA512) {
		hash = crypto.SHA512
	} else {
		return errors.New("unsupported algorithm ID " + aid.String())
	}

	// dump the authenticated attributes to bytes & append to input bytes
	attrs, err := asn1.Marshal(sig.AuthAttrs)
	if err != nil {
		return errors.New("error marshaling authenticated attributes to ASN.1")
	}
	signable := make([]byte, len(b) + len(attrs))
	copy(signable, b)
	copy(signable[len(b):], attrs)

	// hash the signable bytes using the specified hash algorithm
	potato := hash.New()
	potato.Write(signable)
	hashed := potato.Sum(nil)

	// verify the RSA signature
	if err = rsa.VerifyPKCS1v15(pub, hash, hashed, sig.Signature); err != nil {
		return errors.New("signature fails to RSA verify")
	}

	sig.target = sig.AuthAttrs.Target // record for posterity; only meaningful when used by bootloader

	// at this point we know the signature is legit; if Length is wrong here, either the signer fscked up
	// or someone cracked SHA256 but didn't also include Length in the new signed bytes. soooo yeah, most likely
	// the signer fscked up :D
	if len(b) != sig.AuthAttrs.Length {
		return errors.New("authenticated length does not match actual image length")
	}

	return nil
}

func (sig *BootSig) Sign(target string, sc *android.SigningCert) error {
	if err := sc.Resolve(); err != nil {
		return err
	}

	hash := sc.Hash.AsHash()

	// locate the ASN1 identifier for RSA + whatever hash algorithm was specified
	var algID asn1.ObjectIdentifier
	switch hash {
	case crypto.SHA256:
		algID = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	case crypto.SHA512:
		algID = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 13}
	default:
		return errors.New("unsupported hash algorithm")
		// TODO: support more than RSA w/ SHA256/512
	}

	sig.Cert = asn1.RawValue{FullBytes: sc.Certificate.Raw}
	sig.AuthAttrs.Target = target
	sig.AuthAttrs.Length = len(sig.signed)
	sig.AlgorithmIDs.Algorithm = algID

	attrs, err := asn1.Marshal(sig.AuthAttrs)
	if err != nil {
		return err
	}

	signable := make([]byte, len(sig.signed) + len(attrs))
	copy(signable, sig.signed)
	copy(signable[len(sig.signed):], attrs)

	if sig.Signature, err = sc.Sign(signable, hash); err != nil {
		return err
	}

	return nil
}

func (sig *BootSig) Signer() *x509.Certificate {
	cert, err := x509.ParseCertificate(sig.Cert.FullBytes)
	if err != nil {
		return nil
	}

	return cert
}

func (sig *BootSig) Marshal() []byte {
	b, err := asn1.Marshal(sig.BootSigASN1)
	if err != nil {
		log.Debug("BootSig.Marshal", "error marshaling boot signature to ASN.1", err)
		return nil
	}
	return b
}

type BootImage struct {
	bootSig *BootSig
	raw []byte
	cooked []byte
}

func NewBootImage(b []byte) (*BootImage, error) {
	if string(b[:8]) != "ANDROID!" {
		return nil, errors.New("image lacks Android boot image magic")
	}

	img := BootImage{}
	img.raw = make([]byte, len(b))
	copy(img.raw, b)

	l := img.ComputeLength()
	if l < 0 {
		return nil, errors.New("image has invalid header")
	}

	return &img, nil
}

func (img *BootImage) ComputeLength() int {
	/* Android boot image header format:
		Header:
			uint64 - "ANDROID!" (magic)
			int32 - kernel size
			int32 - kernel address
			int32 - ramdisk size
			int32 - ramdisk addr
			int32 - second size
			int32 - second addr
			int32 - tags addr
			int32 - page size
	 */

	var b []byte
	var kernelSize, ramdiskSize, secondSize, pageSize uint32

	b = img.raw[:]

	_, b = pop64(b) // skip "ANDROID!" magic
	kernelSize, b = pop32(b)
	_, b = pop32(b) // don't care about kernel address
	ramdiskSize, b = pop32(b)
	_, b = pop32(b) // don't care about ramdisk address
	secondSize, b = pop32(b)
	_, b = pop32(b) // don't care about second address
	_, b = pop32(b) // don't care about tags
	pageSize, b = pop32(b)

	// kernelSize is its size in literal bytes, but in the image those bytes will be page aligned;
	// same for other size fields. The header block itself is also page aligned.  So the image file
	// size will be greater than the sum of these sizes, and we need to compute that for signing.

	imgLen := pageSize // header
	imgLen += ((kernelSize + pageSize - 1) / pageSize) * pageSize // kernel
	imgLen += ((ramdiskSize + pageSize - 1) / pageSize) * pageSize // ramdisk 
	imgLen += ((secondSize + pageSize - 1) / pageSize) * pageSize // second image

	// this will never result in a change to imgLen's value since it's already a multiple of pageSize, but hey why not
	imgLen = ((imgLen + pageSize - 1) / pageSize) * pageSize

	if imgLen < pageSize || int(imgLen) > len(img.raw) {
		return -1
	}

	return int(imgLen)
}

func (img *BootImage) Verify(cert *x509.Certificate) error {
	// parse the signature block out of img.raw if it doesn't look it has been yet
	if img.bootSig == nil {
		if !img.IsSigned() {
			return errors.New("boot image is not signed")
		}
	}

	// ask the BootSig to verify itself against the signable portion of the boot image bytes
	l := img.ComputeLength()
	if l < 1 {
		return errors.New("boot image length is bogus")
	}
	signable := img.raw[:l]
	if err := img.bootSig.Verify(signable); err != nil {
		return err
	}

	// if caller asked to verify against a particular cert, check it
	if cert != nil {
		wanted := sha256.Sum256(cert.Raw)
		found := sha256.Sum256(img.bootSig.Signer().Raw)
		if !bytes.Equal(wanted[:], found[:]) {
			return errors.New("boot image signature is good but was not signed by the provided cert")
		}
	}

	return nil
}

func (img *BootImage) Sign(target string, sc *android.SigningCert) error {
	l := img.ComputeLength()
	if l > len(img.raw) {
		return errors.New("truncated image")
	}

	// len(img.raw) > l not necessarily an error, since img.raw could already be signed; we strip existing sigs tho

	signable := img.raw[:l]
	img.bootSig = NewBootSig(target, signable)
	if err := img.bootSig.Sign(target, sc); err != nil {
		return err
	}
	sigBytes := img.bootSig.Marshal()

	img.cooked = make([]byte, len(signable) + len(sigBytes))
	copy(img.cooked, signable)
	copy(img.cooked[len(signable):], sigBytes)

	fmt.Println("que?")
	fmt.Println(len(img.cooked), len(sigBytes))

	return nil
}

func (img *BootImage) Marshal() []byte {
	if len(img.cooked) < 1 {
		return nil
	}

	ret := make([]byte, len(img.cooked))
	copy(ret, img.cooked)
	return ret
}

func (img *BootImage) IsSigned() bool {
	// check for truncated boot image -- can't possibly be signed
	l := img.ComputeLength()
	if l >= len(img.raw) {
		log.Debug("BootImage.IsSigned", "computed length > input length")
		return false
	}

	// parse any bytes appended past expected image size to see if they are a boot signature
	var err error
	sigBytes := img.raw[l:]
	if img.bootSig, err = ParseBootSig(sigBytes); err != nil || img.bootSig == nil {
		return false
	}

	// looks like a valid boot signature
	return true
}



// TODO: promote these out of apksign
func pop32(in []byte) (uint32, []byte) {
	return binary.LittleEndian.Uint32(in[:4]), in[4:]
}

func pop64(in []byte) (uint64, []byte) {
	return binary.LittleEndian.Uint64(in[:8]), in[8:]
}

func popN(in []byte, count int) ([]byte, []byte) {
	return in[:count], in[count:]
}

func push32(in []byte) []byte {
	l := uint32(len(in))
	out := make([]byte, l+4)
	binary.LittleEndian.PutUint32(out, l)
	copy(out[4:], in)
	return out
}

func push64(in []byte) []byte {
	l := uint64(len(in))
	out := make([]byte, l+8)
	binary.LittleEndian.PutUint64(out, l)
	copy(out[8:], in)
	return out
}

func concat(blocks ...[]byte) []byte {
	totes := 0
	for _, b := range blocks {
		totes += len(b)
	}
	out := make([]byte, totes)
	cur := out
	for _, b := range blocks {
		copy(cur, b)
		cur = cur[len(b):]
	}
	return out
}
