package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Vérification d'intégrité des artefacts démarrables.
//
// Une micro-VM démarre sur deux fichiers : l'image de son système de fichiers
// et le noyau invité. Les deux sont du code, exécuté avec tous les droits à
// l'intérieur de la VM. Un fichier modifié entre la construction et le
// démarrage donne à qui l'a modifié le contenu entier de la VM, et le pare-feu
// n'y change rien : la VM est autorisée à faire ce que sa zone permet, quel que
// soit le code qu'elle exécute.
//
// Les scripts de construction écrivent déjà l'empreinte SHA-256 de chaque image
// dans ses métadonnées, et l'empreinte du noyau est vérifiée au téléchargement.
// Mais personne ne relisait rien au démarrage : une image modifiée démarrait
// sans un mot.
//
// Il y a deux niveaux, et il faut être précis sur ce que chacun apporte.
//
// Les métadonnées de construction, output/images/<service>.json, détectent une
// image corrompue — copie tronquée, disque fatigué, construction interrompue —
// ou remplacée par quelqu'un qui ignore leur existence. Elles ne protègent pas
// contre un attaquant attentif : le fichier de métadonnées se modifie aussi
// facilement que l'image qu'il décrit.
//
// L'empreinte retenue dans l'état du démon est la vraie barrière. Elle est
// enregistrée à la première utilisation de l'artefact, puis comparée à chaque
// démarrage suivant. Elle vit dans le fichier d'état, écrit par le démon et
// lisible de lui seul : modifier une image dans le catalogue ne suffit plus, il
// faut aussi atteindre l'état. Et changer d'image devient un acte explicite,
// l'opérateur passant -trust.
//
// C'est de la confiance à la première utilisation, le même compromis que les
// clés d'hôte SSH, avec la même limite : la première fois, on fait confiance.
// Une signature du catalogue lèverait cette limite ; elle demande une clé, donc
// une gestion de clé, et c'est le sujet de la distribution des images.

// ErrIntegrity signale un artefact qui ne correspond pas à son empreinte.
// Distincte pour que l'appelant puisse la reconnaître sans lire le message.
var ErrIntegrity = errors.New("intégrité d'artefact")

// Origine d'une empreinte retenue. Un opérateur doit pouvoir savoir ce que la
// vérification vaut sans relire le code.
const (
	// PinSourceMetadata : les métadonnées de construction confirmaient le
	// fichier au moment de l'enregistrement.
	PinSourceMetadata = "métadonnées de construction"
	// PinSourceFirstUse : rien d'indépendant ne confirmait le fichier, on a
	// enregistré ce qui était là.
	PinSourceFirstUse = "première utilisation"
	// PinSourceOperator : un opérateur a explicitement accepté le fichier.
	PinSourceOperator = "acceptée par l'opérateur"
)

// imageMetadata est le sous-ensemble des métadonnées de construction utile
// ici. Les autres champs sont ignorés : ce fichier sert aussi de documentation
// de l'image, et il grandira.
type imageMetadata struct {
	Service string `json:"service"`
	SHA256  string `json:"sha256"`
}

// metadataPath renvoie le chemin des métadonnées de construction d'une image :
// le même nom, avec .json à la place de l'extension.
func metadataPath(imagePath string) string {
	return strings.TrimSuffix(imagePath, filepath.Ext(imagePath)) + ".json"
}

// DeclaredDigest lit l'empreinte annoncée par les métadonnées de construction,
// et renvoie aussi le chemin consulté pour que les messages d'erreur puissent
// le nommer.
//
// Des métadonnées absentes ne sont pas une erreur : le noyau invité n'en a pas,
// et une image construite à la main peut ne pas en avoir. L'empreinte renvoyée
// est alors vide, et l'enregistrement se fait sans confirmation indépendante.
func DeclaredDigest(artifactPath string) (string, string, error) {
	meta := metadataPath(artifactPath)
	data, err := os.ReadFile(meta)
	if errors.Is(err, os.ErrNotExist) {
		return "", meta, nil
	}
	if err != nil {
		return "", meta, fmt.Errorf("lecture des métadonnées %s : %w", meta, err)
	}

	var m imageMetadata
	if err := json.Unmarshal(data, &m); err != nil {
		return "", meta, fmt.Errorf("métadonnées %s illisibles : %w", meta, err)
	}
	return strings.ToLower(strings.TrimSpace(m.SHA256)), meta, nil
}

// FileDigest renvoie l'empreinte SHA-256 d'un fichier, en hexadécimal
// minuscule. Le résultat est mémorisé, voir digestCache.
func FileDigest(path string) (string, error) { return artifactDigests.digest(path) }

// digestCache évite de rehacher le même fichier une fois par VM.
//
// Trente VM issues du même service liraient trente fois la même image de
// 64 Mio : deux gigaoctets de lecture et de hachage au démarrage du démon, ce
// qui se voit sur une machine modeste.
//
// La validité d'une entrée repose sur l'identité du fichier — système de
// fichiers et inode — sur sa taille et sur sa date de modification. Un contenu
// changé en conservant les quatre ne serait pas vu ; il faut déjà être root
// pour y arriver, le prochain démarrage du démon le verrait, et l'empreinte
// retenue dans l'état reste l'autorité.
type digestCache struct {
	mu      sync.Mutex
	entries map[string]digestEntry
}

type digestEntry struct {
	dev, ino uint64
	size     int64
	modNanos int64
	sum      string
}

var artifactDigests = &digestCache{entries: map[string]digestEntry{}}

func (c *digestCache) digest(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("chemin d'artefact %s : %w", path, err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("artefact %s illisible : %w", abs, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("artefact %s : un dossier, pas un fichier", abs)
	}

	want := digestEntry{size: info.Size(), modNanos: info.ModTime().UnixNano()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		want.dev, want.ino = uint64(st.Dev), uint64(st.Ino)
	}

	c.mu.Lock()
	hit, known := c.entries[abs]
	c.mu.Unlock()
	if known && hit.dev == want.dev && hit.ino == want.ino &&
		hit.size == want.size && hit.modNanos == want.modNanos {
		return hit.sum, nil
	}

	sum, err := hashFile(abs)
	if err != nil {
		return "", err
	}
	want.sum = sum

	c.mu.Lock()
	c.entries[abs] = want
	c.mu.Unlock()
	return sum, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("ouverture de l'artefact %s : %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("lecture de l'artefact %s : %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyArtifact compare un artefact aux empreintes retenues.
//
// Elle ne modifie rien : l'empreinte à enregistrer est renvoyée, et c'est à
// l'appelant de l'appliquer une fois que tous ses artefacts sont vérifiés. Une
// VM dont l'image passe et dont le noyau échoue ne doit laisser aucune trace
// derrière elle.
//
// Le booléen dit si l'empreinte est nouvelle. role nomme l'artefact dans les
// messages : « image » ou « noyau ».
func verifyArtifact(pins map[string]Pin, path, role string, now time.Time) (string, Pin, bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", Pin{}, false, fmt.Errorf("chemin du %s %s : %w", role, path, err)
	}

	actual, err := FileDigest(abs)
	if err != nil {
		return abs, Pin{}, false, err
	}

	if pin, known := pins[abs]; known {
		if pin.SHA256 != actual {
			return abs, Pin{}, false, fmt.Errorf(
				"%w : le %s %s ne correspond pas à l'empreinte retenue le %s. Attendu %s, trouvé %s. "+
					"Si ce changement est voulu, l'accepter explicitement avec « tinyvmosd -trust %s » ; "+
					"sinon ce fichier a été modifié depuis son enregistrement et aucune vm ne doit démarrer dessus",
				ErrIntegrity, role, abs, pin.PinnedAt.Format(time.RFC3339),
				pin.SHA256, actual, abs)
		}
		return abs, pin, false, nil
	}

	// Première utilisation de cet artefact. Les métadonnées de construction
	// ne sont pas une autorité — elles se modifient aussi facilement que
	// l'image — mais si elles sont là et qu'elles contredisent le fichier,
	// quelque chose ne va pas et il n'y a aucune raison de démarrer.
	declared, metaPath, err := DeclaredDigest(abs)
	if err != nil {
		return abs, Pin{}, false, err
	}

	source := PinSourceFirstUse
	if declared != "" {
		if declared != actual {
			return abs, Pin{}, false, fmt.Errorf(
				"%w : le %s %s ne correspond pas à l'empreinte annoncée par %s. Attendu %s, trouvé %s. "+
					"Soit le fichier a été modifié depuis sa construction, soit les métadonnées sont périmées : "+
					"reconstruire l'image, ou accepter explicitement le fichier avec « tinyvmosd -trust %s »",
				ErrIntegrity, role, abs, metaPath, declared, actual, abs)
		}
		source = PinSourceMetadata
	}

	return abs, Pin{SHA256: actual, Source: source, PinnedAt: now}, true, nil
}

// PinArtifact enregistre l'empreinte actuelle d'un artefact, en écrasant celle
// qui était retenue.
//
// C'est ce que fait -trust : l'opérateur affirme que le fichier qui est là est
// celui qu'il veut voir démarrer. Cela existe parce qu'une image reconstruite
// change d'empreinte, et que le démon doit alors refuser de démarrer jusqu'à ce
// qu'un humain confirme — c'est précisément l'intérêt du dispositif.
func PinArtifact(snap *Snapshot, path string, now time.Time) (string, Pin, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", Pin{}, fmt.Errorf("chemin d'artefact %s : %w", path, err)
	}
	sum, err := FileDigest(abs)
	if err != nil {
		return abs, Pin{}, err
	}
	if snap.Artifacts == nil {
		snap.Artifacts = map[string]Pin{}
	}
	pin := Pin{SHA256: sum, Source: PinSourceOperator, PinnedAt: now}
	snap.Artifacts[abs] = pin
	return abs, pin, nil
}

// SortedPins renvoie les empreintes retenues triées par chemin, pour un
// affichage stable.
func SortedPins(pins map[string]Pin) []PinnedArtifact {
	out := make([]PinnedArtifact, 0, len(pins))
	for path, pin := range pins {
		out = append(out, PinnedArtifact{Path: path, Pin: pin})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// PinnedArtifact associe un chemin à son empreinte retenue.
type PinnedArtifact struct {
	Path string
	Pin  Pin
}
