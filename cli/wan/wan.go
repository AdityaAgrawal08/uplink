package wan

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/multiformats/go-multihash"
)

type WANPeer struct {
	Host host.Host
	DHT  *dht.IpfsDHT
}

func deriveCID(shareCode string) (cid.Cid, error) {
	sum := sha256.Sum256([]byte(shareCode))
	pref := cid.Prefix{
		Version:  1,
		Codec:    cid.Raw,
		MhType:   multihash.SHA2_256,
		MhLength: -1,
	}
	return pref.Sum(sum[:])
}

func StartWANPeer(ctx context.Context) (*WANPeer, error) {
	h, err := libp2p.New(
		libp2p.ListenAddrStrings(
			"/ip4/0.0.0.0/tcp/0",
			"/ip4/0.0.0.0/udp/0/quic-v1",
		),
		libp2p.NATPortMap(),
	)
	if err != nil {
		return nil, err
	}

	d, err := dht.New(ctx, h)
	if err != nil {
		h.Close()
		return nil, err
	}

	err = d.Bootstrap(ctx)
	if err != nil {
		d.Close()
		h.Close()
		return nil, err
	}

	return &WANPeer{Host: h, DHT: d}, nil
}

func (p *WANPeer) Close() {
	if p.DHT != nil {
		_ = p.DHT.Close()
	}
	if p.Host != nil {
		_ = p.Host.Close()
	}
}

// writeFrame writes a length-prefixed frame (uvarint len + bytes).
func writeFrame(w io.Writer, b []byte) error {
	var lb [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(lb[:], uint64(len(b)))
	if _, err := w.Write(lb[:n]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

// readFrame reads one length-prefixed frame, capped at max bytes.
func readFrame(r io.Reader, max int) ([]byte, error) {
	br, ok := r.(io.ByteReader)
	var uv uint64
	var err error
	if ok {
		uv, err = binary.ReadUvarint(br)
	} else {
		uv, err = binary.ReadUvarint(byteReader{r})
	}
	if err != nil {
		return nil, err
	}
	if uv > uint64(max) {
		return nil, fmt.Errorf("frame too large: %d", uv)
	}
	buf := make([]byte, uv)
	_, err = io.ReadFull(r, buf)
	return buf, err
}

type byteReader struct{ r io.Reader }

func (b byteReader) ReadByte() (byte, error) {
	var one [1]byte
	_, err := io.ReadFull(b.r, one[:])
	return one[0], err
}

func ServeFileWAN(ctx context.Context, shareCode string, filePath string, password string, onComplete func()) error {
	p, err := StartWANPeer(ctx)
	if err != nil {
		return err
	}
	defer p.Close()

	// Pre-stat + hash the file so receivers get an integrity binding.
	// Without this the receiver cannot verify (previously the hash check
	// was unsatisfiable and every WAN download deleted itself).
	fi, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	fileSHA, err := hashFileSHA256(filePath)
	if err != nil {
		return err
	}

	p.Host.SetStreamHandler("/uplink-p2p/1.0.0", func(s network.Stream) {
		defer s.Close()
		_ = s.SetDeadline(time.Now().Add(60 * time.Second))

		codeFrame, err := readFrame(s, 256)
		if err != nil || subtle.ConstantTimeCompare(codeFrame, []byte(shareCode)) != 1 {
			return
		}

		pwFrame, err := readFrame(s, 256)
		if err != nil || subtle.ConstantTimeCompare(pwFrame, []byte(password)) != 1 {
			return
		}

		// Metadata header: filename, size, sha256 (all framed).
		base := filepath.Base(filePath)
		if err := writeFrame(s, []byte(base)); err != nil {
			return
		}
		var sb [8]byte
		binary.BigEndian.PutUint64(sb[:], uint64(fi.Size()))
		if err := writeFrame(s, sb[:]); err != nil {
			return
		}
		if err := writeFrame(s, []byte(fileSHA)); err != nil {
			return
		}

		f, err := os.Open(filePath)
		if err != nil {
			return
		}
		defer f.Close()

		if _, err := io.Copy(s, f); err != nil {
			return
		}

		if onComplete != nil {
			onComplete()
		}
	})

	c, err := deriveCID(shareCode)
	if err != nil {
		return fmt.Errorf("derive CID: %w", err)
	}
	if err := p.DHT.Provide(ctx, c, true); err != nil {
		return fmt.Errorf("DHT provide failed (peer unfindable): %w", err)
	}

	<-ctx.Done()
	return nil
}

func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func DownloadFileWAN(ctx context.Context, shareCode string, dest string, password string, expectedFileSHA256 string, progressCallback func(int64)) error {
	// Bound the whole operation: Connect + stream previously used
	// context.Background() with no deadline and could hang forever.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}
	p, err := StartWANPeer(ctx)
	if err != nil {
		return err
	}
	defer p.Close()

	c, err := deriveCID(shareCode)
	if err != nil {
		return fmt.Errorf("derive CID: %w", err)
	}

	ctxSearch, cancelSearch := context.WithTimeout(ctx, 30*time.Second)
	providers, err := p.DHT.FindProviders(ctxSearch, c)
	cancelSearch()
	if err != nil || len(providers) == 0 {
		return fmt.Errorf("no WAN peer found for share code")
	}

	targetPeer := providers[0]
	connCtx, cancelConn := context.WithTimeout(ctx, 30*time.Second)
	defer cancelConn()
	err = p.Host.Connect(connCtx, targetPeer)
	if err != nil {
		return fmt.Errorf("failed to connect to WAN peer: %w", err)
	}

	streamCtx, cancelStream := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStream()
	s, err := p.Host.NewStream(streamCtx, targetPeer.ID, "/uplink-p2p/1.0.0")
	if err != nil {
		return fmt.Errorf("failed to open stream: %w", err)
	}
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(5 * time.Minute))

	// Framed auth (no 100ms sleep coupling; writes are checked).
	if err := writeFrame(s, []byte(shareCode)); err != nil {
		return fmt.Errorf("auth write share: %w", err)
	}
	if err := writeFrame(s, []byte(password)); err != nil {
		return fmt.Errorf("auth write password: %w", err)
	}

	// Framed metadata header.
	_, err = readFrame(s, 1024)
	if err != nil {
		return fmt.Errorf("read filename header: %w", err)
	}
	sizeFrame, err := readFrame(s, 8)
	if err != nil || len(sizeFrame) != 8 {
		return fmt.Errorf("read size header: %w", err)
	}
	_ = binary.BigEndian.Uint64(sizeFrame)
	shaFrame, err := readFrame(s, 128)
	if err != nil {
		return fmt.Errorf("read sha header: %w", err)
	}
	advertisedSHA := string(shaFrame)
	if expectedFileSHA256 == "" {
		expectedFileSHA256 = advertisedSHA
	} else if advertisedSHA != "" && advertisedSHA != expectedFileSHA256 {
		return fmt.Errorf("WAN metadata hash mismatch")
	}

	// Never truncate the destination directly: stream to .part, verify,
	// then rename. A failed download must not destroy a pre-existing file.
	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}

	h := sha256.New()
	tee := io.TeeReader(s, h)

	buf := make([]byte, 32*1024)
	var totalWritten int64
	streamErr := func() error {
		defer f.Close()
		for {
			nr, readErr := tee.Read(buf)
			if nr > 0 {
				nw, writeErr := f.Write(buf[:nr])
				if writeErr != nil {
					return writeErr
				}
				totalWritten += int64(nw)
				if progressCallback != nil {
					progressCallback(totalWritten)
				}
			}
			if readErr != nil {
				if readErr == io.EOF {
					return nil
				}
				return readErr
			}
		}
	}()
	if streamErr != nil {
		_ = os.Remove(tmp)
		return streamErr
	}

	computedHash := hex.EncodeToString(h.Sum(nil))
	if expectedFileSHA256 != "" && computedHash != expectedFileSHA256 {
		_ = os.Remove(tmp)
		return fmt.Errorf("integrity check failed (SHA-256 mismatch)")
	}

	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	return nil
}
