package tapdance

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"github.com/agl/ed25519/extra25519"
	"golang.org/x/crypto/curve25519"
	mrand "math/rand"

	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

// The key argument should be the AES key, either 16 or 32 bytes
// to select AES-128 or AES-256.
func aesGcmEncrypt(plaintext []byte, key []byte, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	aesGcmCipher, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aesGcmCipher.Seal(nil, iv, plaintext, nil), nil
}

// Tries to get crypto random int in range [min, max]
// In case of crypto failure -- return insecure pseudorandom
func getRandInt(min int, max int) int {
	// I can't believe Golang is making me do that
	// Flashback to awful C/C++ libraries
	diff := max - min
	if diff < 0 {
		Logger().Warningf("fetRandInt(): max is less than min")
		min = max
		diff *= -1
	} else if diff == 0 {
		return min
	}
	var v int64
	err := binary.Read(rand.Reader, binary.LittleEndian, &v)
	if v < 0 {
		v *= -1
	}
	if err != nil {
		Logger().Warningf("Unable to securely get getRandInt(): " + err.Error())
		v = mrand.Int63()
	}
	return min + int(v%int64(diff+1))
}

// returns random duration between min and max in milliseconds
func getRandomDuration(min int, max int) time.Duration {
	return time.Millisecond * time.Duration(getRandInt(min, max))
}

// Get padding of length [minLen, maxLen).
// Distributed in pseudogaussian style.
// Padded using symbol '#'. Known plaintext attacks, anyone?
func getRandPadding(minLen int, maxLen int, smoothness int) string {
	paddingLen := 0
	for j := 0; j < smoothness; j++ {
		paddingLen += getRandInt(minLen, maxLen)
	}
	paddingLen = paddingLen / smoothness

	return strings.Repeat("#", paddingLen)
}

func getRandString(length int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	randString := make([]byte, length)
	for i := range randString {
		randString[i] = alphabet[getRandInt(0, len(alphabet)-1)]
	}
	return string(randString)
}

func obfuscateTag(stegoPayload []byte, stationPubkey []byte) (tag []byte, err error) {
	if len(stationPubkey) != 32 {
		err = errors.New("Unexpected station pubkey length. Expected: 32." +
			" Received: " + strconv.Itoa(len(stationPubkey)) + ".")
		return
	}
	var sharedSecret, clientPrivate, clientPublic, representative [32]byte
	for ok := false; ok != true; {
		var sliceKeyPrivate []byte = clientPrivate[:]
		rand.Read(sliceKeyPrivate)

		clientPrivate[0] &= 248
		clientPrivate[31] &= 127
		clientPrivate[31] |= 64

		ok = extra25519.ScalarBaseMult(&clientPublic, &representative, &clientPrivate)
	}
	var stationPubkeyByte32 [32]byte
	copy(stationPubkeyByte32[:], stationPubkey)
	curve25519.ScalarMult(&sharedSecret, &clientPrivate, &stationPubkeyByte32)

	tagBuf := new(bytes.Buffer) // What we have to encrypt with the shared secret using AES
	tagBuf.Write(representative[:])

	stationPubkeyHash := sha256.Sum256(sharedSecret[:])
	aesKey := stationPubkeyHash[:16]
	aesIv := stationPubkeyHash[16:28]

	encryptedData, err := aesGcmEncrypt(stegoPayload, aesKey, aesIv)
	if err != nil {
		return
	}

	tagBuf.Write(encryptedData)
	tag = tagBuf.Bytes()
	Logger().Debugf("len(tag)", tagBuf.Len())
	return
}

func getMsgWithHeader(msgType msgType, msgBytes []byte) []byte {
	if len(msgBytes) == 0 {
		return nil
	}
	bufSend := new(bytes.Buffer)
	var err error
	switch msgType {
	case msgProtobuf:
		if len(msgBytes) <= int(maxInt16) {
			bufSend.Grow(2 + len(msgBytes)) // to avoid double allocation
			err = binary.Write(bufSend, binary.BigEndian, int16(len(msgBytes)))

		} else {
			bufSend.Grow(2 + 4 + len(msgBytes)) // to avoid double allocation
			bufSend.Write([]byte{0, 0})
			err = binary.Write(bufSend, binary.BigEndian, int32(len(msgBytes)))
		}
	case msgRawData:
		err = binary.Write(bufSend, binary.BigEndian, int16(-len(msgBytes)))
	default:
		panic("getMsgWithHeader() called with msgType: " + strconv.Itoa(int(msgType)))
	}
	if err != nil {
		// shouldn't ever happen
		Logger().Errorln("getMsgWithHeader() failed with error: ", err)
		Logger().Errorln("msgType ", msgType)
		Logger().Errorln("msgBytes ", msgBytes)
	}
	bufSend.Write(msgBytes)
	return bufSend.Bytes()
}

func uint16toInt16(i uint16) int16 {
	pos := int16(i & 32767)
	neg := int16(0)
	if i&32768 != 0 {
		neg = int16(-32768)
	}
	return pos + neg
}

func reverseEncrypt(ciphertext []byte, keyStream []byte) (plaintext string) {
	// our plaintext can be antyhing where x & 0xc0 == 0x40
	// i.e. 64-127 in ascii (@, A-Z, [\]^_`, a-z, {|}~ DEL)
	// This means that we are allowed to choose the last 6 bits
	// of each byte in the ciphertext arbitrarily; the upper 2
	// bits will have to be 01, so that our plaintext ends up
	// in the desired range.
	var ka, kb, kc, kd byte // key stream bytes
	var ca, cb, cc, cd byte // ciphertext bytes
	var pa, pb, pc, pd byte // plaintext bytes
	var sa, sb, sc byte     // secret bytes

	var tagIdx, keystreamIdx int

	for tagIdx < len(ciphertext) {
		ka = keyStream[keystreamIdx]
		kb = keyStream[keystreamIdx+1]
		kc = keyStream[keystreamIdx+2]
		kd = keyStream[keystreamIdx+3]
		keystreamIdx += 4

		// read 3 bytes
		sa = ciphertext[tagIdx]
		sb = ciphertext[tagIdx+1]
		sc = ciphertext[tagIdx+2]
		tagIdx += 3

		// figure out what plaintext needs to be in base64 encode
		ca = (ka & 0xc0) | ((sa & 0xfc) >> 2)                        // 6 bits sa
		cb = (kb & 0xc0) | (((sa & 0x03) << 4) | ((sb & 0xf0) >> 4)) // 2 bits sa, 4 bits sb
		cc = (kc & 0xc0) | (((sb & 0x0f) << 2) | ((sc & 0xc0) >> 6)) // 4 bits sb, 2 bits sc
		cd = (kd & 0xc0) | (sc & 0x3f)                               // 6 bits sc

		// Xor with key_stream, and add on 0x40 so it's in range of allowed
		pa = (ca ^ ka) + 0x40
		pb = (cb ^ kb) + 0x40
		pc = (cc ^ kc) + 0x40
		pd = (cd ^ kd) + 0x40

		plaintext += string(pa)
		plaintext += string(pb)
		plaintext += string(pc)
		plaintext += string(pd)
	}
	return
}

func minInt(a, b int) int {
	if a > b {
		return b
	}
	return a
}

func readAndClose(c net.Conn, readDeadline time.Duration) {
	tinyBuf := []byte{0}
	c.SetReadDeadline(time.Now().Add(readDeadline))
	c.Read(tinyBuf)
	c.Close()
}

func errIsTimeout(err error) bool {
	if err != nil {
		if strings.Contains(err.Error(), ": i/o timeout") || // client timed out
			err.Error() == "EOF" { // decoy timed out
			return true
		}
	}
	return false
}
