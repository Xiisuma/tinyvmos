package vm

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// copyImage recopie une image de micro-VM vers sa copie de travail.
//
// On passe d'abord par cp --reflink=auto --sparse=always, pour deux gains qui
// comptent dès qu'il y a plusieurs VM :
//
//   - reflink : sur un système de fichiers qui le sait faire (btrfs, xfs), la
//     copie est instantanée et ne consomme rien tant que personne n'écrit.
//   - sparse : une image de 64 Mio dont 14 seulement sont occupés ne consomme
//     que ces 14 Mio.
//
// Sans ces deux propriétés, trente VM à 64 Mio coûteraient près de 2 Gio de
// disque pour des images identiques. Un repli en copie Go pure assure que ça
// fonctionne quand cp est absent ou refuse.
func copyImage(src, dst string) error {
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("suppression de l'ancienne copie %s : %w", dst, err)
	}

	if cp, err := exec.LookPath("cp"); err == nil {
		cmd := exec.Command(cp, "--reflink=auto", "--sparse=always", src, dst)
		if out, err := cmd.CombinedOutput(); err == nil {
			return nil
		} else if len(out) > 0 {
			// Pas fatal : on retombe sur la copie Go. On garde la trace de la
			// raison dans l'erreur du repli si celui-ci échoue aussi.
			_ = out
		}
	}

	return copyFileGo(src, dst)
}

func copyFileGo(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("ouverture de %s : %w", src, err)
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat de %s : %w", src, err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("création de %s : %w", dst, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("copie de %s vers %s : %w", src, dst, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("fsync de %s : %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("fermeture de %s : %w", dst, err)
	}

	_ = info
	return nil
}
