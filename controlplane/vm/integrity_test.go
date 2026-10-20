package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ecrireArtefact écrit un fichier et renvoie son chemin absolu.
func ecrireArtefact(t *testing.T, dir, nom, contenu string) string {
	t.Helper()
	chemin := filepath.Join(dir, nom)
	if err := os.WriteFile(chemin, []byte(contenu), 0o600); err != nil {
		t.Fatalf("écriture de %s : %v", chemin, err)
	}
	return chemin
}

func TestFileDigest(t *testing.T) {
	dir := t.TempDir()
	// Empreinte connue de la chaîne vide, pour vérifier qu'on hache bien du
	// SHA-256 et pas autre chose.
	vide := ecrireArtefact(t, dir, "vide.bin", "")
	sum, err := FileDigest(vide)
	if err != nil {
		t.Fatalf("FileDigest : %v", err)
	}
	const attendu = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if sum != attendu {
		t.Fatalf("empreinte de la chaîne vide = %s, attendu %s", sum, attendu)
	}
}

// Le cache ne doit pas renvoyer l'empreinte d'hier pour le fichier
// d'aujourd'hui : c'est exactement le cas que la vérification doit attraper.
func TestFileDigestVoitLeChangement(t *testing.T) {
	dir := t.TempDir()
	chemin := ecrireArtefact(t, dir, "image.ext4", "version un")

	avant, err := FileDigest(chemin)
	if err != nil {
		t.Fatalf("FileDigest : %v", err)
	}

	// La date de modification est en jeu dans la validité du cache : on la
	// force en arrière pour que le test ne dépende pas de la granularité de
	// l'horloge du système de fichiers.
	if err := os.WriteFile(chemin, []byte("version deux, plus longue"), 0o600); err != nil {
		t.Fatalf("réécriture : %v", err)
	}
	passe := time.Now().Add(-time.Hour)
	if err := os.Chtimes(chemin, passe, passe); err != nil {
		t.Fatalf("chtimes : %v", err)
	}

	apres, err := FileDigest(chemin)
	if err != nil {
		t.Fatalf("FileDigest après modification : %v", err)
	}
	if apres == avant {
		t.Fatal("le cache a renvoyé l'empreinte de l'ancien contenu")
	}
}

func TestDeclaredDigest(t *testing.T) {
	dir := t.TempDir()
	image := ecrireArtefact(t, dir, "web-apache.ext4", "contenu")
	ecrireArtefact(t, dir, "web-apache.json", `{"service":"web-apache","sha256":"ABCDEF"}`)

	declaree, meta, err := DeclaredDigest(image)
	if err != nil {
		t.Fatalf("DeclaredDigest : %v", err)
	}
	if declaree != "abcdef" {
		t.Fatalf("empreinte déclarée = %q, attendu abcdef en minuscules", declaree)
	}
	if filepath.Base(meta) != "web-apache.json" {
		t.Fatalf("métadonnées cherchées dans %s", meta)
	}
}

// Le noyau invité n'a pas de métadonnées à côté de lui : leur absence est un
// cas normal, pas une erreur.
func TestDeclaredDigestSansMetadonnees(t *testing.T) {
	dir := t.TempDir()
	noyau := ecrireArtefact(t, dir, "vmlinux", "noyau")

	declaree, _, err := DeclaredDigest(noyau)
	if err != nil {
		t.Fatalf("DeclaredDigest sans métadonnées : %v", err)
	}
	if declaree != "" {
		t.Fatalf("empreinte déclarée = %q, attendu vide", declaree)
	}
}

func TestVerifyArtifactPremiereUtilisation(t *testing.T) {
	dir := t.TempDir()
	noyau := ecrireArtefact(t, dir, "vmlinux", "noyau")
	pins := map[string]Pin{}
	maintenant := time.Now()

	chemin, pin, nouvelle, err := verifyArtifact(pins, noyau, "noyau", maintenant)
	if err != nil {
		t.Fatalf("première utilisation refusée : %v", err)
	}
	if !nouvelle {
		t.Fatal("la première utilisation doit produire une empreinte nouvelle")
	}
	if !filepath.IsAbs(chemin) {
		t.Fatalf("le chemin retenu doit être absolu, reçu %s", chemin)
	}
	if pin.Source != PinSourceFirstUse {
		t.Fatalf("origine = %q, attendu %q", pin.Source, PinSourceFirstUse)
	}
}

// Des métadonnées présentes et concordantes doivent être signalées comme
// telles : c'est la différence entre « confirmé » et « on a pris ce qu'il y
// avait ».
func TestVerifyArtifactMetadonneesConcordantes(t *testing.T) {
	dir := t.TempDir()
	image := ecrireArtefact(t, dir, "web.ext4", "image réelle")
	sum, err := FileDigest(image)
	if err != nil {
		t.Fatalf("FileDigest : %v", err)
	}
	ecrireArtefact(t, dir, "web.json", `{"service":"web","sha256":"`+sum+`"}`)

	_, pin, nouvelle, err := verifyArtifact(map[string]Pin{}, image, "image", time.Now())
	if err != nil {
		t.Fatalf("image concordante refusée : %v", err)
	}
	if !nouvelle {
		t.Fatal("empreinte attendue nouvelle")
	}
	if pin.Source != PinSourceMetadata {
		t.Fatalf("origine = %q, attendu %q", pin.Source, PinSourceMetadata)
	}
}

func TestVerifyArtifactMetadonneesDiscordantes(t *testing.T) {
	dir := t.TempDir()
	image := ecrireArtefact(t, dir, "web.ext4", "image modifiée après construction")
	ecrireArtefact(t, dir, "web.json",
		`{"service":"web","sha256":"0000000000000000000000000000000000000000000000000000000000000000"}`)

	_, _, _, err := verifyArtifact(map[string]Pin{}, image, "image", time.Now())
	if err == nil {
		t.Fatal("une image qui contredit ses métadonnées doit être refusée")
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("erreur = %v, attendu une erreur d'intégrité", err)
	}
	// Le message doit dire quoi faire, pas seulement que c'est refusé.
	if !strings.Contains(err.Error(), "-trust") {
		t.Fatalf("le message doit indiquer la sortie de secours, reçu : %v", err)
	}
}

// Le cas central : une image acceptée une fois, puis modifiée.
func TestVerifyArtifactImageModifieeApresEnregistrement(t *testing.T) {
	dir := t.TempDir()
	image := ecrireArtefact(t, dir, "web.ext4", "contenu légitime")

	chemin, pin, _, err := verifyArtifact(map[string]Pin{}, image, "image", time.Now())
	if err != nil {
		t.Fatalf("premier passage : %v", err)
	}
	pins := map[string]Pin{chemin: pin}

	// Deuxième passage, fichier inchangé : accepté, et pas de nouvelle
	// empreinte à enregistrer.
	_, _, nouvelle, err := verifyArtifact(pins, image, "image", time.Now())
	if err != nil {
		t.Fatalf("deuxième passage sur un fichier inchangé : %v", err)
	}
	if nouvelle {
		t.Fatal("un fichier déjà connu ne doit pas produire une nouvelle empreinte")
	}

	// Modification, comme le ferait quelqu'un qui remplace l'image.
	if err := os.WriteFile(image, []byte("contenu substitué"), 0o600); err != nil {
		t.Fatalf("substitution : %v", err)
	}
	passe := time.Now().Add(-time.Hour)
	if err := os.Chtimes(image, passe, passe); err != nil {
		t.Fatalf("chtimes : %v", err)
	}

	_, _, _, err = verifyArtifact(pins, image, "image", time.Now())
	if err == nil {
		t.Fatal("une image modifiée après enregistrement doit être refusée")
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("erreur = %v, attendu une erreur d'intégrité", err)
	}
}

func TestPinArtifactEcraseLEmpreinte(t *testing.T) {
	dir := t.TempDir()
	image := ecrireArtefact(t, dir, "web.ext4", "nouvelle version")
	abs, err := filepath.Abs(image)
	if err != nil {
		t.Fatalf("Abs : %v", err)
	}

	snap := NewSnapshot()
	snap.Artifacts = map[string]Pin{abs: {SHA256: "périmée", Source: PinSourceFirstUse}}

	chemin, pin, err := PinArtifact(snap, image, time.Now())
	if err != nil {
		t.Fatalf("PinArtifact : %v", err)
	}
	if chemin != abs {
		t.Fatalf("chemin = %s, attendu %s", chemin, abs)
	}
	if pin.Source != PinSourceOperator {
		t.Fatalf("origine = %q, attendu %q", pin.Source, PinSourceOperator)
	}
	if snap.Artifacts[abs].SHA256 == "périmée" {
		t.Fatal("l'empreinte périmée n'a pas été remplacée")
	}

	// Et la vérification passe ensuite, sans rien enregistrer de plus.
	if _, _, nouvelle, err := verifyArtifact(snap.Artifacts, image, "image", time.Now()); err != nil || nouvelle {
		t.Fatalf("après acceptation : erreur %v, nouvelle %v", err, nouvelle)
	}
}

// Une VM dont l'image passe et dont le noyau échoue ne doit laisser aucune
// empreinte derrière elle : sinon la tentative suivante accepterait l'image
// sans la revérifier.
func TestManagerNEnregistreRienSiUnArtefactEchoue(t *testing.T) {
	spec := testSpec(t, "alpha")
	ecrireArtefact(t, filepath.Dir(spec.ImagePath), "rootfs.json",
		`{"service":"web-apache","sha256":"`+mustDigest(t, spec.ImagePath)+`"}`)

	mgr, err := NewManager(NewFakeVMM(), &memStore{}, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}

	// Le noyau est déjà connu, avec une autre empreinte : il sera refusé.
	absNoyau, err := filepath.Abs(spec.KernelPath)
	if err != nil {
		t.Fatalf("Abs : %v", err)
	}
	if err := mgr.WithState(func(snap *Snapshot) error {
		snap.Artifacts = map[string]Pin{absNoyau: {
			SHA256:   "0000000000000000000000000000000000000000000000000000000000000000",
			Source:   PinSourceOperator,
			PinnedAt: time.Now(),
		}}
		return nil
	}); err != nil {
		t.Fatalf("préparation de l'état : %v", err)
	}

	if err := mgr.Create(context.Background(), spec); err == nil {
		t.Fatal("la création doit être refusée quand le noyau ne correspond pas")
	}

	absImage, err := filepath.Abs(spec.ImagePath)
	if err != nil {
		t.Fatalf("Abs : %v", err)
	}
	mgr.ReadState(func(snap *Snapshot) {
		if _, enregistree := snap.Artifacts[absImage]; enregistree {
			t.Error("l'empreinte de l'image a été enregistrée alors que la création a échoué")
		}
	})
}

func mustDigest(t *testing.T, path string) string {
	t.Helper()
	sum, err := FileDigest(path)
	if err != nil {
		t.Fatalf("FileDigest %s : %v", path, err)
	}
	return sum
}
