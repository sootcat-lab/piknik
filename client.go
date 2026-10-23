package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/crypto/chacha20"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/ed25519"
)

const DefaultClientVersion = byte(7)

// Paste errors that only mean there is nothing to paste.
var (
	errClipboardEmpty  = errors.New("the clipboard might be empty")
	errClipboardTooOld = errors.New("clipboard content is too old")
	// Returned instead of downloading content the caller already has.
	errClipboardUnchanged = errors.New("clipboard content is unchanged")
)

// Client - Client data
type Client struct {
	conf    Conf
	conn    net.Conn
	reader  *bufio.Reader
	writer  *bufio.Writer
	version byte

	// knownSignature makes a paste return errClipboardUnchanged instead of
	// downloading content with that signature. The server sends the
	// signature, authenticated, ahead of the content.
	knownSignature []byte
	// signature is set to the signature of the content a copy stored or a
	// paste fetched.
	signature []byte
}

func (client *Client) copyOperation(h1 []byte, in io.Reader) error {
	ts := make([]byte, 8)
	binary.LittleEndian.PutUint64(ts, uint64(time.Now().Unix()))

	conf, reader, writer := client.conf, client.reader, client.writer

	var contentWithEncryptSkIDAndNonceBuf bytes.Buffer
	contentWithEncryptSkIDAndNonceBuf.Grow(8 + 24 + bytes.MinRead)

	contentWithEncryptSkIDAndNonceBuf.Write(conf.EncryptSkID)

	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	contentWithEncryptSkIDAndNonceBuf.Write(nonce)

	if _, err := contentWithEncryptSkIDAndNonceBuf.ReadFrom(in); err != nil {
		return err
	}
	contentWithEncryptSkIDAndNonce := contentWithEncryptSkIDAndNonceBuf.Bytes()

	cipher, err := chacha20.NewUnauthenticatedCipher(conf.EncryptSk, nonce)
	if err != nil {
		return err
	}
	opcode := byte('S')
	cipher.XORKeyStream(contentWithEncryptSkIDAndNonce[8+24:], contentWithEncryptSkIDAndNonce[8+24:])
	signature := ed25519.Sign(conf.SignSk, contentWithEncryptSkIDAndNonce)

	client.conn.SetDeadline(time.Now().Add(conf.DataTimeout))
	h2 := auth2store(conf, client.version, h1, opcode, ts, signature)
	writer.WriteByte(opcode)
	writer.Write(h2)
	ciphertextWithEncryptSkIDAndNonceLen := uint64(len(contentWithEncryptSkIDAndNonce))
	binary.Write(writer, binary.LittleEndian, ciphertextWithEncryptSkIDAndNonceLen)
	writer.Write(ts)
	writer.Write(signature)
	writer.Write(contentWithEncryptSkIDAndNonce)
	if err = writer.Flush(); err != nil {
		return err
	}
	rbuf := make([]byte, 32)
	if _, err = io.ReadFull(reader, rbuf); err != nil {
		if err == io.ErrUnexpectedEOF {
			return errors.New("the server may be running an incompatible version")
		}
		return err
	}
	h3 := rbuf
	wh3 := auth3store(conf, h2)
	if subtle.ConstantTimeCompare(wh3, h3) != 1 {
		return errors.New("incorrect authentication code")
	}
	client.signature = signature
	if IsTerminal(int(syscall.Stderr)) {
		os.Stderr.WriteString("Sent\n")
	}
	return nil
}

func (client *Client) pasteOperation(h1 []byte, isMove bool, out io.Writer) error {
	conf, reader, writer := client.conf, client.reader, client.writer
	opcode := byte('G')
	if isMove {
		opcode = byte('M')
	}
	h2 := auth2get(conf, client.version, h1, opcode)
	writer.WriteByte(opcode)
	writer.Write(h2)
	if err := writer.Flush(); err != nil {
		return err
	}
	rbuf := make([]byte, 112)
	if nbread, err := io.ReadFull(reader, rbuf); err != nil {
		if err == io.ErrUnexpectedEOF {
			if nbread < 80 {
				return errClipboardEmpty
			}
			return errors.New("the server may be running an incompatible version")
		}
		return err
	}
	h3 := rbuf[0:32]
	ciphertextWithEncryptSkIDAndNonceLen := binary.LittleEndian.Uint64(rbuf[32:40])
	ts := rbuf[40:48]
	signature := rbuf[48:112]
	wh3 := auth3get(conf, client.version, h2, ts, signature)
	if subtle.ConstantTimeCompare(wh3, h3) != 1 {
		return errors.New("incorrect authentication code")
	}
	tsRaw := binary.LittleEndian.Uint64(ts)
	if tsRaw > math.MaxInt64 {
		return errors.New("clipboard content timestamp is invalid")
	}
	tsTime := time.Unix(int64(tsRaw), 0)
	now := time.Now()
	if tsTime.After(now.Add(MaxFutureSkew)) {
		return errors.New("clipboard content timestamp is too far in the future")
	}
	if now.Sub(tsTime) >= conf.TTL {
		return errClipboardTooOld
	}
	if ciphertextWithEncryptSkIDAndNonceLen < 8+24 {
		return errors.New("clipboard content is too short")
	}
	if client.knownSignature != nil && bytes.Equal(signature, client.knownSignature) {
		return errClipboardUnchanged
	}
	ciphertextWithEncryptSkIDAndNonce := make([]byte, ciphertextWithEncryptSkIDAndNonceLen)
	client.conn.SetDeadline(time.Now().Add(conf.DataTimeout))
	if _, err := io.ReadFull(reader, ciphertextWithEncryptSkIDAndNonce); err != nil {
		if err == io.ErrUnexpectedEOF {
			return errors.New("the server may be running an incompatible version")
		}
		return err
	}
	encryptSkID := ciphertextWithEncryptSkIDAndNonce[0:8]
	if !bytes.Equal(conf.EncryptSkID, encryptSkID) {
		wEncryptSkIDStr := binary.LittleEndian.Uint64(conf.EncryptSkID)
		encryptSkIDStr := binary.LittleEndian.Uint64(encryptSkID)
		return fmt.Errorf("configured key ID is %v but content was encrypted using key ID %v",
			wEncryptSkIDStr, encryptSkIDStr)
	}
	if !ed25519.Verify(conf.SignPk, ciphertextWithEncryptSkIDAndNonce, signature) {
		return errors.New("signature doesn't verify")
	}
	nonce := ciphertextWithEncryptSkIDAndNonce[8:32]
	cipher, err := chacha20.NewUnauthenticatedCipher(conf.EncryptSk, nonce)
	if err != nil {
		return err
	}
	content := ciphertextWithEncryptSkIDAndNonce[32:]
	cipher.XORKeyStream(content, content)
	client.signature = append([]byte(nil), signature...)
	return binary.Write(out, binary.LittleEndian, content)
}

func (client *Client) pushStreamOperation(h1 []byte, cid string) {
	conf, reader, writer := client.conf, client.reader, client.writer
	opcode := byte('P')
	h2 := auth2get(conf, client.version, h1, opcode)
	writer.WriteByte(opcode)
	writer.Write(h2)
	if err := writer.Flush(); err != nil {
		log.Fatal(err)
	}

	client.conn.SetDeadline(time.Now().Add(conf.Timeout))
	statusBuf := make([]byte, 1)
	if _, err := io.ReadFull(reader, statusBuf); err != nil {
		if err == io.ErrUnexpectedEOF {
			log.Fatal("The server may be running an incompatible version")
		}
		log.Fatal(err)
	}
	switch statusBuf[0] {
	case 0x01:
	case 0x00:
		log.Fatal("No clients are waiting to receive the stream")
	case 0x02:
		log.Fatal("Another push is already active")
	default:
		log.Fatal("Server rejected the stream")
	}

	cidBytes := []byte(cid)

	ts := make([]byte, 8)
	binary.LittleEndian.PutUint64(ts, uint64(time.Now().Unix()))

	noncePrefix := make([]byte, 16)
	if _, err := rand.Read(noncePrefix); err != nil {
		log.Fatal(err)
	}

	streamKey := deriveStreamKey(conf.EncryptSk, ts, conf.EncryptSkID, noncePrefix, cidBytes)
	aead, err := chacha20poly1305.NewX(streamKey)
	if err != nil {
		log.Fatal(err)
	}

	cidBind := computeCIDBind(conf.EncryptSk, cidBytes)
	transcript := newTranscriptHash()
	transcript.Write([]byte{client.version})
	transcript.Write([]byte{opcode})
	transcript.Write(ts)
	transcript.Write(conf.EncryptSkID)
	transcript.Write(noncePrefix)
	transcript.Write(cidBind)

	header := make([]byte, 32)
	copy(header[0:8], ts)
	copy(header[8:16], conf.EncryptSkID)
	copy(header[16:32], noncePrefix)

	client.conn.SetDeadline(time.Now().Add(conf.DataTimeout))
	writer.Write(header)
	if err := writer.Flush(); err != nil {
		log.Fatal(err)
	}

	plainBuf := make([]byte, MaxChunk)
	var chunkIndex uint64

	for {
		n, readErr := io.ReadAtLeast(os.Stdin, plainBuf, 1)
		if n > 0 {
			nonce := deriveChunkNonce(noncePrefix, chunkIndex)
			sealed := aead.Seal(nil, nonce, plainBuf[:n], nil)

			chunkLen := uint32(n)
			lenBuf := make([]byte, 4)
			binary.LittleEndian.PutUint32(lenBuf, chunkLen)

			idxBuf := make([]byte, 8)
			binary.LittleEndian.PutUint64(idxBuf, chunkIndex)
			transcript.Write(idxBuf)
			transcript.Write(lenBuf)
			transcript.Write(sealed)

			client.conn.SetDeadline(time.Now().Add(conf.DataTimeout))
			writer.Write(lenBuf)
			writer.Write(sealed)
			if err := writer.Flush(); err != nil {
				log.Fatal(err)
			}
			chunkIndex++
		}
		if readErr != nil {
			if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
				break
			}
			log.Fatal(readErr)
		}
	}

	transcriptDigest := transcript.Sum(nil)
	signature := ed25519.Sign(conf.SignSk, transcriptDigest)

	endMarker := make([]byte, 4)
	client.conn.SetDeadline(time.Now().Add(conf.DataTimeout))
	writer.Write(endMarker)
	writer.Write(signature)
	if err := writer.Flush(); err != nil {
		log.Fatal(err)
	}

	if IsTerminal(int(syscall.Stderr)) {
		os.Stderr.WriteString("Stream sent\n")
	}
}

func (client *Client) pullStreamOperation(h1 []byte, cid string) {
	conf, reader, writer := client.conf, client.reader, client.writer
	opcode := byte('L')
	h2 := auth2get(conf, client.version, h1, opcode)
	writer.WriteByte(opcode)
	writer.Write(h2)
	if err := writer.Flush(); err != nil {
		log.Fatal(err)
	}

	client.conn.SetDeadline(time.Now().Add(conf.Timeout))
	statusBuf := make([]byte, 1)
	if _, err := io.ReadFull(reader, statusBuf); err != nil {
		if err == io.ErrUnexpectedEOF {
			log.Fatal("The server may be running an incompatible version")
		}
		log.Fatal(err)
	}
	switch statusBuf[0] {
	case 0x01:
	case 0x00:
		log.Fatal("A stream is already being transferred - try again later")
	case 0x02:
		log.Fatal("Too many clients are already waiting to receive a stream")
	default:
		log.Fatal("Server rejected the stream pull request")
	}

	cidBytes := []byte(cid)

	client.conn.SetDeadline(time.Time{})

	header := make([]byte, 32)
	if _, err := io.ReadFull(reader, header); err != nil {
		log.Fatal("Stream: failed to read header: ", err)
	}

	ts := header[0:8]
	encryptSkID := header[8:16]
	noncePrefix := header[16:32]

	tsRaw := binary.LittleEndian.Uint64(ts)
	if tsRaw > uint64(math.MaxInt64) {
		log.Fatal("Stream rejected: invalid timestamp")
	}
	tsVal := int64(tsRaw)
	now := time.Now().Unix()
	maxFutureSeconds := int64(MaxFutureSkew / time.Second)
	ttlSeconds := int64(conf.TTL / time.Second)
	if tsVal > now {
		if tsVal-now > maxFutureSeconds {
			log.Fatal("Stream rejected: timestamp too far in the future")
		}
	} else {
		if now-tsVal > ttlSeconds {
			log.Fatal("Stream rejected: timestamp too old")
		}
	}

	if !bytes.Equal(conf.EncryptSkID, encryptSkID) {
		wEncryptSkIDStr := binary.LittleEndian.Uint64(conf.EncryptSkID)
		encryptSkIDStr := binary.LittleEndian.Uint64(encryptSkID)
		log.Fatalf("Configured key ID is %v but stream was encrypted using key ID %v",
			wEncryptSkIDStr, encryptSkIDStr)
	}

	streamKey := deriveStreamKey(conf.EncryptSk, ts, encryptSkID, noncePrefix, cidBytes)
	aead, err := chacha20poly1305.NewX(streamKey)
	if err != nil {
		log.Fatal(err)
	}

	cidBind := computeCIDBind(conf.EncryptSk, cidBytes)
	transcript := newTranscriptHash()
	transcript.Write([]byte{client.version})
	transcript.Write([]byte{'P'})
	transcript.Write(ts)
	transcript.Write(encryptSkID)
	transcript.Write(noncePrefix)
	transcript.Write(cidBind)

	_ = writer

	var chunkIndex uint64
	var totalBytes uint64
	pullStart := time.Now()
	maxBytes := DefaultMaxStreamBytes
	if conf.MaxStreamBytes > 0 && conf.MaxStreamBytes < maxBytes {
		maxBytes = conf.MaxStreamBytes
	}
	maxDur := DefaultMaxStreamDur
	if conf.MaxStreamDuration > 0 && conf.MaxStreamDuration < maxDur {
		maxDur = conf.MaxStreamDuration
	}

	for {
		client.conn.SetDeadline(time.Now().Add(conf.DataTimeout))
		var chunkLen uint32
		if err := binary.Read(reader, binary.LittleEndian, &chunkLen); err != nil {
			log.Fatal("Stream: failed to read chunk length: ", err)
		}

		if chunkLen == 0 {
			sig := make([]byte, 64)
			if _, err := io.ReadFull(reader, sig); err != nil {
				log.Fatal("Stream: failed to read signature: ", err)
			}
			transcriptDigest := transcript.Sum(nil)
			if !ed25519.Verify(conf.SignPk, transcriptDigest, sig) {
				log.Fatal("Stream signature verification failed")
			}
			return
		}

		if chunkLen > MaxChunk {
			log.Fatalf("Stream: chunk too large (%v > %v)", chunkLen, MaxChunk)
		}

		sealedLen := int(chunkLen) + 16
		totalBytes += uint64(sealedLen)
		if totalBytes > maxBytes {
			log.Fatal("Stream rejected: exceeded maximum stream size")
		}
		if time.Since(pullStart) > maxDur {
			log.Fatal("Stream rejected: exceeded maximum stream duration")
		}
		sealed := make([]byte, sealedLen)
		client.conn.SetDeadline(time.Now().Add(conf.DataTimeout))
		if _, err := io.ReadFull(reader, sealed); err != nil {
			log.Fatal("Stream: failed to read chunk data: ", err)
		}

		nonce := deriveChunkNonce(noncePrefix, chunkIndex)
		plain, err := aead.Open(nil, nonce, sealed, nil)
		if err != nil {
			log.Fatal("Stream: AEAD authentication failed for chunk ", chunkIndex)
		}

		lenBuf := make([]byte, 4)
		binary.LittleEndian.PutUint32(lenBuf, chunkLen)
		idxBuf := make([]byte, 8)
		binary.LittleEndian.PutUint64(idxBuf, chunkIndex)
		transcript.Write(idxBuf)
		transcript.Write(lenBuf)
		transcript.Write(sealed)

		if _, err := os.Stdout.Write(plain); err != nil {
			log.Fatal("Stream: write error: ", err)
		}
		chunkIndex++
	}
}

func RunClient(conf Conf, isCopy bool, isMove bool, isPush bool, isPull bool, cid string) {
	if err := runClientIO(conf, isCopy, isMove, isPush, isPull, cid, os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func runClientIO(conf Conf, isCopy bool, isMove bool, isPush bool, isPull bool, cid string, in io.Reader, out io.Writer) error {
	clientVersion := byte(7)
	if !isPush && !isPull {
		clientVersion = byte(6)
	}
	client, h1, err := dialClient(conf, clientVersion)
	if err != nil {
		return err
	}
	defer client.conn.Close()

	switch {
	case isCopy:
		return client.copyOperation(h1, in)
	case isPush:
		client.pushStreamOperation(h1, cid)
		return nil
	case isPull:
		client.pullStreamOperation(h1, cid)
		return nil
	default:
		return client.pasteOperation(h1, isMove, out)
	}
}

// dialClient connects to the server and authenticates. It returns the
// client, whose connection the caller must close, and h1.
func dialClient(conf Conf, clientVersion byte) (*Client, []byte, error) {
	conn, err := net.DialTimeout("tcp", conf.Connect, conf.Timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to connect to %v - is a Piknik server running on that host?", conf.Connect)
	}
	client, h1, err := handshake(conf, conn, clientVersion)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return client, h1, nil
}

func handshake(conf Conf, conn net.Conn, clientVersion byte) (*Client, []byte, error) {
	conn.SetDeadline(time.Now().Add(conf.Timeout))
	reader, writer := bufio.NewReader(conn), bufio.NewWriter(conn)
	client := &Client{
		conf:    conf,
		conn:    conn,
		reader:  reader,
		writer:  writer,
		version: clientVersion,
	}
	r := make([]byte, 32)
	if _, err := rand.Read(r); err != nil {
		return nil, nil, err
	}
	h0 := auth0(conf, client.version, r)
	writer.Write([]byte{client.version})
	writer.Write(r)
	writer.Write(h0)
	if err := writer.Flush(); err != nil {
		return nil, nil, err
	}
	rbuf := make([]byte, 65)
	if nbread, err := io.ReadFull(reader, rbuf); err != nil {
		if nbread < 2 {
			return nil, nil, errors.New("the server rejected the connection - check that it is running the same Piknik version or retry later")
		}
		return nil, nil, errors.New("the server doesn't support this protocol")
	}
	if serverVersion := rbuf[0]; serverVersion != client.version {
		return nil, nil, fmt.Errorf("incompatible server version (client version: %v - server version: %v)",
			client.version, serverVersion)
	}
	r2 := rbuf[1:33]
	h1 := rbuf[33:65]
	wh1 := auth1(conf, client.version, h0, r2)
	if subtle.ConstantTimeCompare(wh1, h1) != 1 {
		return nil, nil, errors.New("incorrect authentication code")
	}
	return client, h1, nil
}
