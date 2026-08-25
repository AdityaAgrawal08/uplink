package wan

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"os"
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

func ServeFileWAN(ctx context.Context, shareCode string, filePath string, password string, onComplete func()) error {
	p, err := StartWANPeer(ctx)
	if err != nil {
		return err
	}
	defer p.Close()

	p.Host.SetStreamHandler("/uplink-p2p/1.0.0", func(s network.Stream) {
		defer s.Close()

		buf := make([]byte, 256)
		n, err := s.Read(buf)
		if err != nil || subtle.ConstantTimeCompare(buf[:n], []byte(shareCode)) != 1 {
			return
		}

		n, err = s.Read(buf)
		if err != nil || subtle.ConstantTimeCompare(buf[:n], []byte(password)) != 1 {
			return
		}

		f, err := os.Open(filePath)
		if err != nil {
			return
		}
		defer f.Close()

		_, _ = io.Copy(s, f)

		if onComplete != nil {
			onComplete()
		}
	})

	c, err := deriveCID(shareCode)
	if err == nil {
		_ = p.DHT.Provide(ctx, c, true)
	}

	<-ctx.Done()
	return nil
}

func DownloadFileWAN(ctx context.Context, shareCode string, dest string, password string, expectedFileSHA256 string, progressCallback func(int64)) error {
	p, err := StartWANPeer(ctx)
	if err != nil {
		return err
	}
	defer p.Close()

	c, err := deriveCID(shareCode)
	if err != nil {
		return nil
	}

	ctxSearch, cancelSearch := context.WithTimeout(ctx, 10*time.Second)
	providers, err := p.DHT.FindProviders(ctxSearch, c)
	cancelSearch()
	if err != nil || len(providers) == 0 {
		return fmt.Errorf("no WAN peer found for share code")
	}

	targetPeer := providers[0]
	err = p.Host.Connect(ctx, targetPeer)
	if err != nil {
		return fmt.Errorf("failed to connect to WAN peer: %w", err)
	}

	s, err := p.Host.NewStream(ctx, targetPeer.ID, "/uplink-p2p/1.0.0")
	if err != nil {
		return fmt.Errorf("failed to open stream: %w", err)
	}
	defer s.Close()

	_, _ = s.Write([]byte(shareCode))
	time.Sleep(100 * time.Millisecond)
	_, _ = s.Write([]byte(password))

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	tee := io.TeeReader(s, h)

	buf := make([]byte, 32*1024)
	var totalWritten int64
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
				break
			}
			return readErr
		}
	}

	computedHash := hex.EncodeToString(h.Sum(nil))
	if computedHash != expectedFileSHA256 {
		os.Remove(dest)
		return fmt.Errorf("integrity check failed (SHA-256 mismatch)")
	}

	return nil
}
