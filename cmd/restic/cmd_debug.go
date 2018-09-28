// +build debug

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/spf13/cobra"

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

func init() {
	cmdRoot.AddCommand(cmdDebug)
	cmdDebug.AddCommand(cmdDebugDump)
	cmdDebug.AddCommand(cmdDebugExamine)
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

func loadBlobs(ctx context.Context, repo restic.Repository, pack string, list []restic.PackedBlob) error {
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

		nonce, buf := buf[:key.NonceSize()], buf[key.NonceSize():]
		buf, err = key.Open(buf[:0], nonce, buf, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error decrypting blob: %v\n", err)
			continue
		}

		id := restic.Hash(buf)
		fmt.Printf("         successfully decrypted blob (length %v), hash is %v\n", len(buf), id)
		if !id.Equal(blob.ID) {
			fmt.Printf("         IDs do not match, want %v, got %v\n", blob.ID, id)
		} else {
			fmt.Printf("         IDs match\n")
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

		// examine all data the indexes have for the pack file
		for _, idx := range repo.Index().(*repository.MasterIndex).All() {
			idxID, err := idx.ID()
			if err != nil {
				idxID = restic.ID{}
			}

			blobs := idx.ListPack(id)
			if len(blobs) == 0 {
				fmt.Printf("    index %v does not contain the file\n", idxID.Str())
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

			err = loadBlobs(gopts.ctx, repo, name, blobs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}
		}

		// inspect the pack file itself
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
	}
	return nil
}
