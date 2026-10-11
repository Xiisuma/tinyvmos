package vm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Paramètres de la temporisation croissante appliquée aux relances.
//
// Une VM qui boucle sur un crash ne doit ni saturer la machine ni remplir le
// disque de journaux. Le délai double à chaque échec, plafonné, et repart de
// zéro dès que la VM a tenu assez longtemps pour être considérée comme stable.
const (
	restartBackoffMin   = 1 * time.Second
	restartBackoffMax   = 60 * time.Second
	restartStableWindow = 60 * time.Second
)

// ErrUnknownVM est renvoyée pour une VM absente de l'état.
var ErrUnknownVM = errors.New("vm inconnue")

// Manager tient le cycle de vie des VM et l'état constaté.
//
// Il est l'unique écrivain du magasin d'état : c'est ce qui permet de garder
// un fichier JSON réécrit de façon atomique plutôt qu'une base de données.
type Manager struct {
	vmm   VMM
	store Store
	log   *slog.Logger
	clock func() time.Time

	mu    sync.Mutex
	snap  *Snapshot
	super map[string]*supervision
}

type supervision struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// NewManager charge l'état existant et renvoie un gestionnaire prêt à l'emploi.
func NewManager(vmm VMM, store Store, log *slog.Logger) (*Manager, error) {
	if log == nil {
		log = slog.Default()
	}
	snap, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &Manager{
		vmm:   vmm,
		store: store,
		log:   log,
		clock: time.Now,
		snap:  snap,
		super: map[string]*supervision{},
	}, nil
}

// persistLocked écrit l'état. Appelée une fois par opération publique, jamais
// une fois par VM : c'est la règle qui rend le fichier JSON tenable.
// Le verrou doit être tenu.
func (m *Manager) persistLocked() error {
	return m.store.Save(m.snap)
}

// verifyArtifactsLocked refuse de préparer une VM dont l'image ou le noyau ne
// correspond pas à son empreinte retenue.
//
// Elle est appelée avant vmm.Create, donc avant que l'artefact ne soit copié
// dans le dossier de travail de la VM : c'est le fichier du catalogue qui est
// vérifié, celui qui serait modifié. Vérifier la copie n'aurait rien prouvé,
// puisque c'est nous qui venons de la faire.
//
// Les deux artefacts sont vérifiés avant qu'aucune empreinte ne soit
// enregistrée. Une VM dont l'image passe et dont le noyau échoue ne doit rien
// laisser derrière elle, sans quoi la prochaine tentative accepterait l'image
// sans la revérifier.
//
// Le verrou doit être tenu.
func (m *Manager) verifyArtifactsLocked(spec Spec) error {
	if m.snap.Artifacts == nil {
		m.snap.Artifacts = map[string]Pin{}
	}

	now := m.clock()
	fresh := map[string]Pin{}

	for _, artefact := range []struct{ role, path string }{
		{"image", spec.ImagePath},
		{"noyau", spec.KernelPath},
	} {
		path, pin, isNew, err := verifyArtifact(m.snap.Artifacts, artefact.path, artefact.role, now)
		if err != nil {
			return fmt.Errorf("vm %s : %w", spec.Name, err)
		}
		if !isNew {
			m.log.Debug("artefact vérifié",
				"vm", spec.Name, "artefact", artefact.role,
				"chemin", path, "empreinte", pin.SHA256)
			continue
		}
		fresh[path] = pin

		// La différence entre les deux cas compte : dans le premier, quelque
		// chose d'indépendant confirme le fichier ; dans le second, on
		// enregistre ce qui était là, et on le dit.
		if pin.Source == PinSourceMetadata {
			m.log.Info("empreinte d'artefact enregistrée, confirmée par les métadonnées de construction",
				"vm", spec.Name, "artefact", artefact.role,
				"chemin", path, "empreinte", pin.SHA256)
		} else {
			m.log.Warn("empreinte d'artefact enregistrée sans confirmation indépendante : aucune métadonnée de construction à côté du fichier",
				"vm", spec.Name, "artefact", artefact.role,
				"chemin", path, "empreinte", pin.SHA256)
		}
	}

	for path, pin := range fresh {
		m.snap.Artifacts[path] = pin
	}
	return nil
}

// Create enregistre une VM et la prépare auprès de l'hyperviseur.
func (m *Manager) Create(ctx context.Context, spec Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.snap.VMs[spec.Name]; exists {
		return fmt.Errorf("vm %s déjà déclarée", spec.Name)
	}
	if err := m.verifyArtifactsLocked(spec); err != nil {
		return err
	}
	if err := m.vmm.Create(ctx, spec); err != nil {
		return err
	}

	now := m.clock()
	m.snap.VMs[spec.Name] = &Record{
		Spec:      spec,
		Status:    StatusCreated,
		CreatedAt: now,
		UpdatedAt: now,
	}
	return m.persistLocked()
}

// Ensure déclare une VM si elle est inconnue, ou reprend celle que l'état
// connaît déjà, et la prépare auprès de l'hyperviseur.
//
// C'est le point d'entrée au démarrage du démon. Create ne suffit pas : après
// un redémarrage, l'état contient la VM mais l'hyperviseur du nouveau processus
// ne la connaît pas, et Start échouerait sur « vm inconnue ».
//
// Un statut resté à running vient forcément d'un démon tué sans arrêt propre :
// rien ne tourne plus, et le remettre à stopped est la seule lecture honnête.
func (m *Manager) Ensure(ctx context.Context, spec Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}

	m.mu.Lock()
	if err := m.verifyArtifactsLocked(spec); err != nil {
		m.mu.Unlock()
		return err
	}
	rec, known := m.snap.VMs[spec.Name]
	if known {
		rec.Spec = spec
		if rec.Status == StatusRunning || rec.Status == StatusStarting || rec.Status == StatusStopping {
			m.log.Info("vm reprise d'un démon précédent, statut remis à l'arrêt",
				"vm", spec.Name, "ancien_statut", rec.Status)
			rec.Status = StatusStopped
			rec.PID = 0
		}
		rec.UpdatedAt = m.clock()
	}
	m.mu.Unlock()

	if !known {
		return m.Create(ctx, spec)
	}

	if err := m.vmm.Create(ctx, spec); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.persistLocked()
}

// Start démarre une VM et met en place sa supervision.
func (m *Manager) Start(ctx context.Context, name string) error {
	m.mu.Lock()
	rec, ok := m.snap.VMs[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w : %s", ErrUnknownVM, name)
	}
	if rec.Status == StatusRunning {
		m.mu.Unlock()
		return fmt.Errorf("vm %s déjà démarrée", name)
	}
	if !rec.Status.CanTransitionTo(StatusStarting) {
		m.mu.Unlock()
		return fmt.Errorf("vm %s : transition %s vers %s interdite", name, rec.Status, StatusStarting)
	}
	rec.Status = StatusStarting
	rec.UpdatedAt = m.clock()
	m.mu.Unlock()

	err := m.vmm.Start(ctx, name)

	m.mu.Lock()
	defer m.mu.Unlock()

	if err != nil {
		rec.Status = StatusFailed
		rec.LastError = err.Error()
		rec.UpdatedAt = m.clock()
		_ = m.persistLocked()
		return err
	}

	rec.Status = StatusRunning
	rec.LastError = ""
	rec.UpdatedAt = m.clock()
	if met, merr := m.vmm.Metrics(ctx, name); merr == nil {
		rec.PID = met.PID
	}

	m.superviseLocked(name)
	return m.persistLocked()
}

// superviseLocked lance la goroutine qui surveille une VM démarrée.
// Le verrou doit être tenu.
func (m *Manager) superviseLocked(name string) {
	if _, running := m.super[name]; running {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.super[name] = &supervision{cancel: cancel, done: done}
	go m.supervise(ctx, name, done)
}

// supervise attend la fin de la VM et la relance si son arrêt n'a pas été
// demandé. Le délai entre deux relances double à chaque échec.
func (m *Manager) supervise(ctx context.Context, name string, done chan struct{}) {
	defer close(done)

	backoff := restartBackoffMin

	for {
		waitErr := m.vmm.Wait(ctx, name)

		if ctx.Err() != nil {
			return // arrêt demandé par le gestionnaire
		}

		m.mu.Lock()
		rec, ok := m.snap.VMs[name]
		if !ok {
			m.mu.Unlock()
			return // VM supprimée entre-temps
		}

		// Un arrêt volontaire met le statut à stopping avant de tuer la VM :
		// dans ce cas il n'y a rien à relancer.
		if rec.Status == StatusStopping || rec.Status == StatusStopped {
			m.mu.Unlock()
			return
		}

		uptime := m.clock().Sub(rec.UpdatedAt)
		if uptime >= restartStableWindow {
			backoff = restartBackoffMin
		}

		cause := "arrêt inattendu de la vm"
		if waitErr != nil {
			cause = waitErr.Error()
		}
		rec.Status = StatusFailed
		rec.LastError = cause
		rec.PID = 0
		rec.UpdatedAt = m.clock()
		restart := rec.Spec.Restart
		_ = m.persistLocked()
		m.mu.Unlock()

		// cause et restart sont des copies : rec ne doit plus être lu hors du
		// verrou, sinon le détecteur de course a raison de râler.
		m.log.Warn("vm arrêtée sans demande",
			"vm", name, "cause", cause, "durée_de_vie", uptime.Round(time.Millisecond))

		if !restart {
			return
		}

		m.log.Info("relance programmée", "vm", name, "délai", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		if next := backoff * 2; next <= restartBackoffMax {
			backoff = next
		} else {
			backoff = restartBackoffMax
		}

		if err := m.restart(ctx, name); err != nil {
			m.log.Error("relance en échec", "vm", name, "erreur", err)
			// La boucle repart : Wait rendra la main aussitôt et la
			// temporisation s'allongera.
			continue
		}
	}
}

// restart relance une VM après un arrêt non demandé.
func (m *Manager) restart(ctx context.Context, name string) error {
	m.mu.Lock()
	rec, ok := m.snap.VMs[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w : %s", ErrUnknownVM, name)
	}
	rec.Status = StatusStarting
	rec.Restarts++
	rec.UpdatedAt = m.clock()
	_ = m.persistLocked()
	m.mu.Unlock()

	err := m.vmm.Start(ctx, name)

	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		rec.Status = StatusFailed
		rec.LastError = err.Error()
		rec.UpdatedAt = m.clock()
		_ = m.persistLocked()
		return err
	}
	rec.Status = StatusRunning
	rec.LastError = ""
	rec.UpdatedAt = m.clock()
	if met, merr := m.vmm.Metrics(ctx, name); merr == nil {
		rec.PID = met.PID
	}
	m.log.Info("vm relancée", "vm", name, "relances", rec.Restarts)
	return m.persistLocked()
}

// Stop arrête une VM et coupe sa supervision, pour que l'arrêt ne déclenche
// pas une relance.
func (m *Manager) Stop(ctx context.Context, name string) error {
	m.mu.Lock()
	rec, ok := m.snap.VMs[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w : %s", ErrUnknownVM, name)
	}
	rec.Status = StatusStopping
	rec.UpdatedAt = m.clock()
	sup := m.super[name]
	delete(m.super, name)
	m.mu.Unlock()

	if sup != nil {
		sup.cancel()
	}

	err := m.vmm.Stop(ctx, name)

	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		rec.Status = StatusFailed
		rec.LastError = err.Error()
	} else {
		rec.Status = StatusStopped
		rec.LastError = ""
	}
	rec.PID = 0
	rec.UpdatedAt = m.clock()
	return m.persistLocked()
}

// Delete arrête puis oublie une VM.
func (m *Manager) Delete(ctx context.Context, name string) error {
	m.mu.Lock()
	_, ok := m.snap.VMs[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w : %s", ErrUnknownVM, name)
	}

	if err := m.Stop(ctx, name); err != nil {
		m.log.Warn("arrêt avant suppression en échec", "vm", name, "erreur", err)
	}
	if err := m.vmm.Delete(ctx, name); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.snap.VMs, name)
	// L'identifiant est rendu, pour que la plage ne s'épuise pas au fil des
	// créations et suppressions.
	if m.snap.Identities != nil {
		delete(m.snap.Identities, name)
	}
	return m.persistLocked()
}

// IdentityFor renvoie l'identifiant confiné d'une VM, en lui en attribuant un
// si elle n'en a pas encore.
//
// Appelé avant la création de l'interface tap : c'est cette identité qui doit
// posséder le tap, sans quoi Firecracker confiné ne pourra pas l'ouvrir.
func (m *Manager) IdentityFor(name string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.snap.Identities == nil {
		m.snap.Identities = map[string]int{}
	}
	if id, ok := m.snap.Identities[name]; ok {
		if err := ValidateIdentity(id); err != nil {
			return 0, fmt.Errorf("vm %s : identité enregistrée invalide : %w", name, err)
		}
		return id, nil
	}

	id, err := AllocateIdentity(identitiesTaken(m.snap.Identities, name))
	if err != nil {
		return 0, err
	}
	m.snap.Identities[name] = id
	if err := m.persistLocked(); err != nil {
		return 0, err
	}
	m.log.Info("identité confinée attribuée", "vm", name, "uid", id, "gid", id)
	return id, nil
}

// Identities renvoie une copie de la table des identités.
func (m *Manager) Identities() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.snap.Identities))
	for k, v := range m.snap.Identities {
		out[k] = v
	}
	return out
}

// WithState donne un accès verrouillé à l'état et le persiste une seule fois
// si la fonction réussit.
//
// C'est le point d'entrée pour tout ce qui vit dans l'état sans être une VM :
// les zones et les attributions d'adresses. Une écriture par appel, jamais une
// par élément modifié — c'est la règle qui rend le magasin JSON tenable.
func (m *Manager) WithState(fn func(*Snapshot) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := fn(m.snap); err != nil {
		return err
	}
	return m.persistLocked()
}

// ReadState donne un accès verrouillé à l'état en lecture seule.
func (m *Manager) ReadState(fn func(*Snapshot)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m.snap)
}

// Get renvoie une copie de l'état d'une VM.
func (m *Manager) Get(name string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.snap.VMs[name]
	if !ok {
		return Record{}, fmt.Errorf("%w : %s", ErrUnknownVM, name)
	}
	return *rec, nil
}

// List renvoie l'état de toutes les VM, trié par nom pour que la sortie soit
// stable d'un appel à l'autre.
func (m *Manager) List() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Record, 0, len(m.snap.VMs))
	for _, rec := range m.snap.VMs {
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out
}

// StopAll arrête toutes les VM que le démon pilote encore. Appelée à son
// extinction.
//
// Une VM en échec est incluse, et pas seulement celles qui tournent : elle est
// peut-être dans une boucle de relance, et la laisser derrière signifierait un
// superviseur qui redémarre des VM après l'arrêt du démon.
func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.Lock()
	names := make([]string, 0, len(m.snap.VMs))
	for name, rec := range m.snap.VMs {
		_, supervised := m.super[name]
		switch {
		case rec.Status == StatusRunning,
			rec.Status == StatusStarting,
			rec.Status == StatusStopping,
			rec.Status == StatusFailed,
			supervised:
			names = append(names, name)
		}
	}
	m.mu.Unlock()
	sort.Strings(names)

	var firstErr error
	for _, name := range names {
		if err := m.Stop(ctx, name); err != nil {
			m.log.Error("arrêt en échec", "vm", name, "erreur", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
