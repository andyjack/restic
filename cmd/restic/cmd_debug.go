// xbuild debug

package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/restic/restic/internal/backend"
	"github.com/restic/restic/internal/crypto"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/pack"
	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/restic"
)

var cmdDebug = &cobra.Command{
	Use:   "debug",
	Short: "Debug commands",
}

var cmdDebugDump = &cobra.Command{
	Use:   "dump [indexes|snapshots|all|packs]",
	Short: "Dump data structures",
	Long: `
The "dump" command dumps data structures from the repository as JSON objects. It
is used for debugging purposes only.`,
	DisableAutoGenTag: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDebugDump(globalOptions, args)
	},
}

var tryRepair bool
var repairByte bool

func init() {
	cmdRoot.AddCommand(cmdDebug)
	cmdDebug.AddCommand(cmdDebugDump)
	cmdDebug.AddCommand(cmdDebugExamine)
	cmdDebugExamine.Flags().BoolVar(&tryRepair, "try-repair", false, "try to repair broken blobs with single bit flips")
	cmdDebugExamine.Flags().BoolVar(&repairByte, "repair-byte", false, "try to repair broken blobs by trying bytes")
}

func prettyPrintJSON(wr io.Writer, item interface{}) error {
	buf, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return err
	}

	_, err = wr.Write(append(buf, '\n'))
	return err
}

func debugPrintSnapshots(repo *repository.Repository, wr io.Writer) error {
	return repo.List(context.TODO(), restic.SnapshotFile, func(id restic.ID, size int64) error {
		snapshot, err := restic.LoadSnapshot(context.TODO(), repo, id)
		if err != nil {
			return err
		}

		fmt.Fprintf(wr, "snapshot_id: %v\n", id)

		return prettyPrintJSON(wr, snapshot)
	})
}

// Pack is the struct used in printPacks.
type Pack struct {
	Name string `json:"name"`

	Blobs []Blob `json:"blobs"`
}

// Blob is the struct used in printPacks.
type Blob struct {
	Type   restic.BlobType `json:"type"`
	Length uint            `json:"length"`
	ID     restic.ID       `json:"id"`
	Offset uint            `json:"offset"`
}

func printPacks(repo *repository.Repository, wr io.Writer) error {

	return repo.List(context.TODO(), restic.DataFile, func(id restic.ID, size int64) error {
		h := restic.Handle{Type: restic.DataFile, Name: id.String()}

		blobs, err := pack.List(repo.Key(), restic.ReaderAt(repo.Backend(), h), size)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error for pack %v: %v\n", id.Str(), err)
			return nil
		}

		p := Pack{
			Name:  id.String(),
			Blobs: make([]Blob, len(blobs)),
		}
		for i, blob := range blobs {
			p.Blobs[i] = Blob{
				Type:   blob.Type,
				Length: blob.Length,
				ID:     blob.ID,
				Offset: blob.Offset,
			}
		}

		return prettyPrintJSON(os.Stdout, p)
	})
}

func dumpIndexes(repo restic.Repository) error {
	return repo.List(context.TODO(), restic.IndexFile, func(id restic.ID, size int64) error {
		fmt.Printf("index_id: %v\n", id)

		idx, err := repository.LoadIndex(context.TODO(), repo, id)
		if err != nil {
			return err
		}

		return idx.Dump(os.Stdout)
	})
}

func runDebugDump(gopts GlobalOptions, args []string) error {
	if len(args) != 1 {
		return errors.Fatal("type not specified")
	}

	repo, err := OpenRepository(gopts)
	if err != nil {
		return err
	}

	if !gopts.NoLock {
		lock, err := lockRepo(repo)
		defer unlockRepo(lock)
		if err != nil {
			return err
		}
	}

	err = repo.LoadIndex(gopts.ctx)
	if err != nil {
		return err
	}

	tpe := args[0]

	switch tpe {
	case "indexes":
		return dumpIndexes(repo)
	case "snapshots":
		return debugPrintSnapshots(repo, os.Stdout)
	case "packs":
		return printPacks(repo, os.Stdout)
	case "all":
		fmt.Printf("snapshots:\n")
		err := debugPrintSnapshots(repo, os.Stdout)
		if err != nil {
			return err
		}

		fmt.Printf("\nindexes:\n")
		err = dumpIndexes(repo)
		if err != nil {
			return err
		}

		return nil
	default:
		return errors.Fatalf("no such type %q", tpe)
	}
}

var cmdDebugExamine = &cobra.Command{
	Use:               "examine",
	Short:             "Examine a pack file",
	DisableAutoGenTag: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDebugExamine(globalOptions, args)
	},
}

func tryRepairWithBitflip(ctx context.Context, key *crypto.Key, input []byte, bytewise bool) {
	if bytewise {
		fmt.Printf("        trying to repair blob by finding a broken byte\n")
	} else {
		fmt.Printf("        trying to repair blob with single bit flip\n")
	}

	ch := make(chan int)
	var wg errgroup.Group
	done := make(chan struct{})

	fmt.Printf("         spinning up %d worker functions\n", runtime.NumCPU())
	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Go(func() error {
			// make a local copy of the buffer
			buf := make([]byte, len(input))
			copy(buf, input)

			for {
				select {
				case <-done:
					return nil
				case i := <-ch:
					if bytewise {
						for j := 0; j < 255; j++ {
							// flip bits
							buf[i] ^= byte(j)

							nonce, plaintext := buf[:key.NonceSize()], buf[key.NonceSize():]
							plaintext, err := key.Open(plaintext[:0], nonce, plaintext, nil)
							if err == nil {
								fmt.Printf("\n")
								fmt.Printf("        blob could be repaired by XORing byte %v with 0x%02x\n", i, j)
								fmt.Printf("        hash is %v\n", restic.Hash(plaintext))
								close(done)
								return nil
							}

							// flip bits back
							buf[i] ^= byte(j)
						}
					} else {
						for j := 0; j < 7; j++ {
							// flip bit
							buf[i] ^= (1 << uint(j))

							nonce, plaintext := buf[:key.NonceSize()], buf[key.NonceSize():]
							plaintext, err := key.Open(plaintext[:0], nonce, plaintext, nil)
							if err == nil {
								fmt.Printf("\n")
								fmt.Printf("        blob could be repaired by flipping bit %v in byte %v\n", j, i)
								fmt.Printf("        hash is %v\n", restic.Hash(plaintext))
								close(done)
								return nil
							}

							// flip bit back
							buf[i] ^= (1 << uint(j))
						}
					}
				}
			}
		})
	}

	start := time.Now()
	info := time.Now()
outer:
	for i := range input {
		select {
		case ch <- i:
		case <-done:
			fmt.Printf("     done after %v\n", time.Since(start))
			break outer
		}

		if time.Since(info) > time.Second {
			secs := time.Since(start).Seconds()
			gps := float64(i) / secs
			remaining := len(input) - i
			eta := time.Duration(float64(remaining)/gps) * time.Second

			fmt.Printf("\r%d byte of %d done (%.2f%%), %.0f byte per second, ETA %v",
				i, len(input), float32(i)/float32(len(input)*100),
				gps, eta)
			info = time.Now()
		}
	}

	var found bool
	select {
	case <-done:
		found = true
	default:
		close(done)
	}

	wg.Wait()

	if !found {
		fmt.Printf("\n        blob could not be repaired by single bit flip\n")
	}
}

func sliceForAppend(in []byte, n int) (head, tail []byte) {
	if total := len(in) + n; cap(in) >= total {
		head = in[:total]
	} else {
		head = make([]byte, total)
		copy(head, in)
	}
	tail = head[len(in):]
	return
}

func decryptUnsigned(ctx context.Context, k *crypto.Key, buf []byte) []byte {
	// strip signature at the end
	l := len(buf)
	nonce, ct := buf[:16], buf[16:l-16]
	dst := make([]byte, 0, len(ct))

	ret, out := sliceForAppend(dst, len(ct))

	c, err := aes.NewCipher(k.EncryptionKey[:])
	if err != nil {
		panic(fmt.Sprintf("unable to create cipher: %v", err))
	}
	e := cipher.NewCTR(c, nonce)
	e.XORKeyStream(out, ct)

	return ret
}

func loadBlobs(ctx context.Context, repo restic.Repository, pack string, list []restic.Blob) error {
	be := repo.Backend()
	for _, blob := range list {
		fmt.Printf("      loading blob %v at %v (length %v)\n", blob.ID, blob.Offset, blob.Length)
		buf := make([]byte, blob.Length)
		h := restic.Handle{
			Name: pack,
			Type: restic.DataFile,
		}
		err := be.Load(ctx, h, int(blob.Length), int64(blob.Offset), func(rd io.Reader) error {
			n, err := io.ReadFull(rd, buf)
			if err != nil {
				fmt.Fprintf(os.Stderr, "read error after %d bytes: %v\n", n, err)
				return err
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error read: %v\n", err)
			continue
		}

		key := repo.Key()

		nonce, plaintext := buf[:key.NonceSize()], buf[key.NonceSize():]
		plaintext, err = key.Open(plaintext[:0], nonce, plaintext, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error decrypting blob: %v\n", err)
			if tryRepair || repairByte {
				tryRepairWithBitflip(ctx, key, buf, repairByte)
			}
			plain := decryptUnsigned(ctx, key, buf)
			filename := fmt.Sprintf("%s.bin", blob.ID.String())
			f, err := os.Create(filename)
			if err != nil {
				return err
			}

			_, err = f.Write(plain)
			if err != nil {
				_ = f.Close()
				return err
			}

			err = f.Close()
			if err != nil {
				return err
			}

			fmt.Printf("decrypt of blob %v stored at %v\n", blob.ID.Str(), filename)
			continue
		}

		id := restic.Hash(plaintext)
		if !id.Equal(blob.ID) {
			fmt.Printf("         successfully decrypted blob (length %v), hash is %v, ID does not match, wanted %v\n", len(plaintext), id, blob.ID)
		} else {
			fmt.Printf("         successfully decrypted blob (length %v), hash is %v, ID matches\n", len(plaintext), id)
		}
	}

	return nil
}

func runDebugExamine(gopts GlobalOptions, args []string) error {
	repo, err := OpenRepository(gopts)
	if err != nil {
		return err
	}

	if !gopts.NoLock {
		lock, err := lockRepo(repo)
		defer unlockRepo(lock)
		if err != nil {
			return err
		}
	}

	err = repo.LoadIndex(gopts.ctx)
	if err != nil {
		return err
	}

	blobsLoaded := false
	for _, name := range args {
		fmt.Printf("examine %v\n", name)
		id, err := restic.ParseID(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}

		h := restic.Handle{
			Type: restic.DataFile,
			Name: name,
		}
		fi, err := repo.Backend().Stat(gopts.ctx, h)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}

		fmt.Printf("  file size is %v\n", fi.Size)

		buf, err := backend.LoadAll(gopts.ctx, repo.Backend(), h)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}

		gotID := restic.Hash(buf)
		if !id.Equal(gotID) {
			fmt.Printf("  wanted hash %v, got %v\n", id, gotID)
		} else {
			fmt.Printf("  hash for file content matches\n")
		}

		fmt.Printf("  ========================================\n")
		fmt.Printf("  looking for info in the indexes\n")

		// examine all data the indexes have for the pack file
		for _, idx := range repo.Index().(*repository.MasterIndex).All() {
			idxID, err := idx.ID()
			if err != nil {
				idxID = restic.ID{}
			}

			blobs := idx.ListPack(id)
			if len(blobs) == 0 {
				continue
			}

			fmt.Printf("    index %v:\n", idxID.Str())

			// track current size and offset
			var size, offset uint64

			sort.Slice(blobs, func(i, j int) bool {
				return blobs[i].Offset < blobs[j].Offset
			})

			for _, pb := range blobs {
				fmt.Printf("      %v blob %v, offset %-6d, raw length %-6d\n", pb.Type, pb.ID, pb.Offset, pb.Length)
				if offset != uint64(pb.Offset) {
					fmt.Printf("      hole in file, want offset %v, got %v\n", offset, pb.Offset)
				}
				offset += uint64(pb.Length)
				size += uint64(pb.Length)
			}

			// compute header size, per blob: 1 byte type, 4 byte length, 32 byte id
			size += uint64(restic.CiphertextLength(len(blobs) * (1 + 4 + 32)))
			// length in uint32 little endian
			size += 4

			if uint64(fi.Size) != size {
				fmt.Printf("      file sizes do not match: computed %v from index, file size is %v\n", size, fi.Size)
			} else {
				fmt.Printf("      file sizes match\n")
			}

			// convert list of blobs to []restic.Blob
			var list []restic.Blob
			for _, b := range blobs {
				list = append(list, b.Blob)
			}

			err = loadBlobs(gopts.ctx, repo, name, list)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			} else {
				blobsLoaded = true
			}
		}

		fmt.Printf("  ========================================\n")
		fmt.Printf("  inspect the pack itself\n")

		blobs, err := pack.List(repo.Key(), restic.ReaderAt(repo.Backend(), h), fi.Size)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error for pack %v: %v\n", id.Str(), err)
			return nil
		}

		// track current size and offset
		var size, offset uint64

		sort.Slice(blobs, func(i, j int) bool {
			return blobs[i].Offset < blobs[j].Offset
		})

		for _, pb := range blobs {
			fmt.Printf("      %v blob %v, offset %-6d, raw length %-6d\n", pb.Type, pb.ID, pb.Offset, pb.Length)
			if offset != uint64(pb.Offset) {
				fmt.Printf("      hole in file, want offset %v, got %v\n", offset, pb.Offset)
			}
			offset += uint64(pb.Length)
			size += uint64(pb.Length)
		}

		// compute header size, per blob: 1 byte type, 4 byte length, 32 byte id
		size += uint64(restic.CiphertextLength(len(blobs) * (1 + 4 + 32)))
		// length in uint32 little endian
		size += 4

		if uint64(fi.Size) != size {
			fmt.Printf("      file sizes do not match: computed %v from index, file size is %v\n", size, fi.Size)
		} else {
			fmt.Printf("      file sizes match\n")
		}

		if !blobsLoaded {
			err = loadBlobs(gopts.ctx, repo, name, blobs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}
		}
	}
	return nil
}
