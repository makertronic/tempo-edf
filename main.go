package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gen2brain/beeep"
	"github.com/getlantern/systray"
)

const (
	defaultAPIURL  = "https://www.api-couleur-tempo.fr/api"
	defaultTimeout = 10 * time.Second
	maxBodySize    = 1 << 20 // 1 Mo max pour les réponses API
	refreshInterval = 30 * time.Minute
)

var (
	logger     *slog.Logger
	httpClient = &http.Client{Timeout: defaultTimeout}
	config     = Config{
		APIURL:   defaultAPIURL,
		Timeout:  defaultTimeout,
		CacheTTL: 30 * time.Minute,
	}

	dataMu      sync.RWMutex
	currentData = &Data{}
	cacheMu     sync.RWMutex
	cache       = make(map[string]*cacheEntry)

	// Icônes chargées dynamiquement en fonction de l'OS
	icnBlanc []byte
	icnRouge []byte
	icnBleu  []byte

	// Windows-specific
	exePath     string
	startupItem *systray.MenuItem

	// Contexte d'annulation pour les goroutines
	cancelFunc context.CancelFunc
)

// Config contient les configurations de l'application
type Config struct {
	APIURL   string
	Timeout  time.Duration
	CacheTTL time.Duration
}

// Data stocke les données tempo actuelles
type Data struct {
	CurrentTarif  float64
	TarifLib      string
	TodayColor    string
	TomorrowColor string
	LastUpdated   time.Time
	Horaire       string
}

// snapshot retourne une copie thread-safe des données
func (d *Data) snapshot() Data {
	dataMu.RLock()
	defer dataMu.RUnlock()
	return Data{
		CurrentTarif:  d.CurrentTarif,
		TarifLib:      d.TarifLib,
		TodayColor:    d.TodayColor,
		TomorrowColor: d.TomorrowColor,
		LastUpdated:   d.LastUpdated,
		Horaire:       d.Horaire,
	}
}

// TempoResponse représente la réponse de l'API Tempo
type TempoResponse struct {
	DateJour string `json:"dateJour"`
	CodeJour int    `json:"codeJour"`
	Periode  string `json:"periode"`
}

// NowResponse représente la réponse de l'API now
type NowResponse struct {
	ApplicableIn int     `json:"applicableIn"`
	CodeCouleur  int     `json:"codeCouleur"`
	CodeHoraire  int     `json:"codeHoraire"`
	TarifKwh     float64 `json:"tarifKwh"`
	LibTarif     string  `json:"libTarif"`
}

// cacheEntry représente une entrée de cache avec sa durée de validité
type cacheEntry struct {
	data    []byte
	expires time.Time
}

func init() {
	// Se placer dans le répertoire de l'exécutable pour que les chemins
	// relatifs (assets/) fonctionnent même au démarrage Windows
	// (où le working directory est C:\Windows\System32)
	if path, err := os.Executable(); err == nil {
		exePath = path
		exeDir := filepath.Dir(path)
		_ = os.Chdir(exeDir)
	}

	// Logger vers fichier (pas de console en mode -H windowsgui)
	logFile, err := os.OpenFile("tempo_edf.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		// Fallback stdout si impossible d'ouvrir le fichier
		logFile = os.Stdout
	}
	logger = slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	icnBlanc = loadAssetForOS("white", "blanc")
	icnRouge = loadAssetForOS("red", "rouge")
	icnBleu = loadAssetForOS("blue", "bleu")
}

// loadAssetForOS charge un asset d'icône avec le bon format selon l'OS.
// englishName est le nom du fichier (ex: "white"), frenchName le fallback (ex: "blanc").
func loadAssetForOS(englishName, frenchName string) []byte {
	var ext string
	switch runtime.GOOS {
	case "windows":
		ext = ".ico"
	default:
		ext = ".png"
	}

	candidates := []string{
		fmt.Sprintf("assets/%s%s", englishName, ext),
		fmt.Sprintf("assets/%s%s", frenchName, ext),
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return data
		}
	}
	logger.Warn("Aucun asset trouvé", "name", englishName)
	return nil
}

func main() {
	logger.Info("Tempo EDF démarré")
	systray.Run(onReady, onExit)
}

func onReady() {
	ctx, cancel := context.WithCancel(context.Background())
	cancelFunc = cancel

	systray.SetTitle("Tempo EDF")
	systray.SetTooltip("Tempo EDF - Couleur du jour")

	// Chargement initial des données
	updateData()
	updateIconBasedOnColor()

	snap := currentData.snapshot()
	sendNotification("Tempo EDF", fmt.Sprintf("Couleur d'aujourd'hui : %s - Tarif : %.3f€/kWh - %s",
		snap.TodayColor, snap.CurrentTarif, snap.Horaire), "")

	// Créer les éléments de menu
	todayItem := systray.AddMenuItem(fmt.Sprintf("Aujourd'hui : %s", snap.TodayColor), "Couleur d'aujourd'hui")
	tomorrowItem := systray.AddMenuItem(fmt.Sprintf("Demain : %s", snap.TomorrowColor), "Couleur de demain")
	tarifsItem := systray.AddMenuItem(fmt.Sprintf("Tarif actuel : %.3f€/kWh - %s", snap.CurrentTarif, snap.Horaire), "Tarifs Tempo EDF")
	horaireItem := systray.AddMenuItem(fmt.Sprintf("Horaire : %s", snap.Horaire), "Horaire actuel (pleines/creuses)")
	refreshItem := systray.AddMenuItem("Rafraîchir", "Rafraîchir les données")

	if exePath != "" && (runtime.GOOS == "windows" || runtime.GOOS == "darwin") {
		checked := isInStartup()
		label := "Démarrer avec Windows"
		if runtime.GOOS == "darwin" {
			label = "Démarrer avec macOS"
		}
		startupItem = systray.AddMenuItemCheckbox(label, "Lancer au démarrage du système", checked)
	}

	systray.AddSeparator()
	quitItem := systray.AddMenuItem("Quitter", "Quitter l'application")

	go handleMenuClicks(ctx, todayItem, tomorrowItem, tarifsItem, horaireItem, refreshItem, quitItem)
	go scheduleMidnightNotification(ctx, todayItem, tomorrowItem, tarifsItem, horaireItem)
	go schedulePeriodicRefresh(ctx, todayItem, tomorrowItem, tarifsItem, horaireItem)

	logger.Info("Interface système prête")
}

func onExit() {
	if cancelFunc != nil {
		cancelFunc()
	}
	logger.Info("Tempo EDF arrêté")
}

func handleMenuClicks(ctx context.Context, todayItem, tomorrowItem, tarifsItem, horaireItem, refreshItem, quitItem *systray.MenuItem) {
	// Canal factice si startupItem est nil (non-Windows)
	var startupCh <-chan struct{}
	if startupItem != nil {
		startupCh = startupItem.ClickedCh
	} else {
		startupCh = make(chan struct{}) // bloqué à jamais, jamais sélectionné
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-todayItem.ClickedCh:
			snap := currentData.snapshot()
			sendNotification("Tempo EDF", fmt.Sprintf("Aujourd'hui : %s", snap.TodayColor), "")
		case <-tomorrowItem.ClickedCh:
			snap := currentData.snapshot()
			sendNotification("Tempo EDF", fmt.Sprintf("Demain : %s", snap.TomorrowColor), "")
		case <-tarifsItem.ClickedCh:
			snap := currentData.snapshot()
			sendNotification("Tempo EDF", fmt.Sprintf("Tarif actuel : %.3f€/kWh - %s - %s", snap.CurrentTarif, snap.TarifLib, snap.Horaire), "")
		case <-horaireItem.ClickedCh:
			snap := currentData.snapshot()
			sendNotification("Tempo EDF", fmt.Sprintf("Horaire actuel : %s", snap.Horaire), "")
		case <-refreshItem.ClickedCh:
			updateData()
			updateMenuItems(todayItem, tomorrowItem, tarifsItem, horaireItem)
			updateIconBasedOnColor()
			snap := currentData.snapshot()
			sendNotification("Tempo EDF", fmt.Sprintf("Données mises à jour : %s - Tarif : %.3f€/kWh - %s",
				snap.TodayColor, snap.CurrentTarif, snap.Horaire), "")
		case <-startupCh:
			handleStartupToggle()
		case <-quitItem.ClickedCh:
			systray.Quit()
			return
		}
	}
}

func handleStartupToggle() {
	if startupItem == nil {
		return
	}
	if startupItem.Checked() {
		if err := removeFromStartup(); err != nil {
			logger.Error("Erreur suppression démarrage", "error", err)
			sendNotification("Tempo EDF", "Erreur lors de la suppression du démarrage", "")
		} else {
			startupItem.Uncheck()
			sendNotification("Tempo EDF", "Application supprimée du démarrage", "")
		}
	} else {
		if err := addToStartup(); err != nil {
			logger.Error("Erreur ajout démarrage", "error", err)
			sendNotification("Tempo EDF", "Erreur lors de l'ajout au démarrage", "")
		} else {
			startupItem.Check()
			sendNotification("Tempo EDF", "Application ajoutée au démarrage", "")
		}
	}
}

func updateMenuItems(todayItem, tomorrowItem, tarifsItem, horaireItem *systray.MenuItem) {
	snap := currentData.snapshot()
	todayItem.SetTitle(fmt.Sprintf("Aujourd'hui : %s", snap.TodayColor))
	tomorrowItem.SetTitle(fmt.Sprintf("Demain : %s", snap.TomorrowColor))
	tarifsItem.SetTitle(fmt.Sprintf("Tarif actuel : %.3f€/kWh - %s", snap.CurrentTarif, snap.Horaire))
	horaireItem.SetTitle(fmt.Sprintf("Horaire : %s", snap.Horaire))
}

func updateData() {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		updateColors()
	}()

	go func() {
		defer wg.Done()
		updateCurrentTarif()
	}()

	wg.Wait()

	dataMu.Lock()
	currentData.LastUpdated = time.Now()
	dataMu.Unlock()
}

func codeToColor(code int) string {
	switch code {
	case 1:
		return "BLEU"
	case 2:
		return "BLANC"
	case 3:
		return "ROUGE"
	default:
		return "INCONNU"
	}
}

func updateColors() {
	logger.Debug("Récupération de la couleur d'aujourd'hui")

	today, err := fetch[TempoResponse](fmt.Sprintf("%s/jourTempo/today", config.APIURL))
	if err != nil {
		logger.Error("Erreur récupération couleur aujourd'hui", "error", err)
		dataMu.Lock()
		currentData.TodayColor = "ERREUR"
		dataMu.Unlock()
		return
	}

	todayColor := codeToColor(today.CodeJour)
	logger.Info("Couleur d'aujourd'hui", "color", todayColor)

	logger.Debug("Récupération de la couleur de demain")

	tomorrow, err := fetch[TempoResponse](fmt.Sprintf("%s/jourTempo/tomorrow", config.APIURL))
	if err != nil {
		logger.Error("Erreur récupération couleur demain", "error", err)
		dataMu.Lock()
		currentData.TodayColor = todayColor
		currentData.TomorrowColor = "ERREUR"
		dataMu.Unlock()
		return
	}

	tomorrowColor := codeToColor(tomorrow.CodeJour)
	logger.Info("Couleur de demain", "color", tomorrowColor)

	dataMu.Lock()
	currentData.TodayColor = todayColor
	currentData.TomorrowColor = tomorrowColor
	dataMu.Unlock()
}

func updateCurrentTarif() {
	logger.Debug("Récupération du tarif actuel")

	now, err := fetch[NowResponse](fmt.Sprintf("%s/now", config.APIURL))
	if err != nil {
		logger.Error("Erreur récupération tarif", "error", err)
		dataMu.Lock()
		currentData.CurrentTarif = 0
		currentData.TarifLib = "Erreur"
		currentData.Horaire = "Inconnu"
		dataMu.Unlock()
		return
	}

	var horaire string
	switch now.CodeHoraire {
	case 0:
		horaire = "Heures Creuses"
	case 1:
		horaire = "Heures Pleines"
	default:
		horaire = "Inconnu"
	}

	dataMu.Lock()
	currentData.CurrentTarif = now.TarifKwh
	currentData.TarifLib = now.LibTarif
	currentData.Horaire = horaire
	dataMu.Unlock()

	logger.Info("Tarif actuel", "tarif", now.TarifKwh, "libelle", now.LibTarif, "horaire", horaire)
}

// fetch utilise le cache si disponible, sinon fait une requête HTTP
func fetch[T any](url string) (T, error) {
	cacheMu.RLock()
	entry, exists := cache[url]
	if exists && time.Now().Before(entry.expires) {
		logger.Debug("Cache hit", "url", url)
		data := entry.data
		cacheMu.RUnlock()
		var result T
		if err := json.Unmarshal(data, &result); err != nil {
			var zero T
			return zero, fmt.Errorf("erreur parsing cache: %w", err)
		}
		return result, nil
	}
	cacheMu.RUnlock()

	logger.Debug("Requête HTTP", "url", url)

	resp, err := httpClient.Get(url)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("erreur HTTP GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var zero T
		return zero, fmt.Errorf("HTTP %d %s pour %s", resp.StatusCode, resp.Status, url)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		var zero T
		return zero, fmt.Errorf("erreur lecture réponse: %w", err)
	}

	var result T
	if err = json.Unmarshal(body, &result); err != nil {
		var zero T
		return zero, fmt.Errorf("erreur parsing JSON: %w", err)
	}

	cacheMu.Lock()
	cache[url] = &cacheEntry{
		data:    body,
		expires: time.Now().Add(config.CacheTTL),
	}
	cacheMu.Unlock()

	return result, nil
}

func sendNotification(title, message, appIcon string) {
	logger.Debug("Envoi notification", "title", title, "message", message)
	if err := beeep.Notify(title, message, appIcon); err != nil {
		logger.Error("Erreur notification", "error", err)
	}
}

func scheduleMidnightNotification(ctx context.Context, todayItem, tomorrowItem, tarifsItem, horaireItem *systray.MenuItem) {
	for {
		now := time.Now()
		nextMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
		duration := nextMidnight.Sub(now)
		logger.Debug("Attente jusqu'au prochain minuit", "duration", duration)

		select {
		case <-ctx.Done():
			return
		case <-time.After(duration):
		}

		updateData()
		updateMenuItems(todayItem, tomorrowItem, tarifsItem, horaireItem)
		updateIconBasedOnColor()
		snap := currentData.snapshot()
		sendNotification("Tempo EDF", fmt.Sprintf("Nouveau jour : %s - Tarif : %.3f€/kWh - %s",
			snap.TodayColor, snap.CurrentTarif, snap.Horaire), "")
	}
}

// schedulePeriodicRefresh met à jour les données périodiquement
func schedulePeriodicRefresh(ctx context.Context, todayItem, tomorrowItem, tarifsItem, horaireItem *systray.MenuItem) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logger.Debug("Rafraîchissement périodique")
			updateData()
			updateMenuItems(todayItem, tomorrowItem, tarifsItem, horaireItem)
			updateIconBasedOnColor()
		}
	}
}

// updateIconBasedOnColor met à jour l'icône du systray selon la couleur du jour
func updateIconBasedOnColor() {
	snap := currentData.snapshot()
	icon := colorToIcon(snap.TodayColor)
	if len(icon) > 0 {
		systray.SetIcon(icon)
		logger.Info("Icône mise à jour", "color", snap.TodayColor)
	} else {
		logger.Warn("Icône non définie, utilisation fallback blanc")
		if len(icnBlanc) > 0 {
			systray.SetIcon(icnBlanc)
		}
	}
}

// colorToIcon retourne les données d'icône pour une couleur Tempo donnée
func colorToIcon(colorName string) []byte {
	switch strings.ToUpper(strings.TrimSpace(colorName)) {
	case "BLANC":
		return icnBlanc
	case "ROUGE":
		return icnRouge
	case "BLEU":
		return icnBleu
	default:
		return icnBlanc
	}
}

// --- Startup management (Windows + macOS) ---

const macOSPlistName = "com.tempoedf.plist"

func macOSPlistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", macOSPlistName)
}

func isInStartup() bool {
	if exePath == "" {
		return false
	}
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("reg", "query", `HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\Run`, "/v", "TempoEDF")
		return cmd.Run() == nil
	case "darwin":
		path := macOSPlistPath()
		if path == "" {
			return false
		}
		_, err := os.Stat(path)
		return err == nil
	default:
		return false
	}
}

func addToStartup() error {
	if exePath == "" {
		return errors.New("chemin de l'exécutable non détecté")
	}
	switch runtime.GOOS {
	case "windows":
		quotedPath := `"` + exePath + `"`
		cmd := exec.Command("reg", "add", `HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\Run`, "/v", "TempoEDF", "/t", "REG_SZ", "/d", quotedPath, "/f")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("échec ajout au démarrage: %w", err)
		}
	case "darwin":
		path := macOSPlistPath()
		if path == "" {
			return errors.New("impossible de trouver ~/Library/LaunchAgents")
		}
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.tempoedf</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>WorkingDirectory</key>
	<string>%s</string>
</dict>
</plist>`, exePath, filepath.Dir(exePath))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return fmt.Errorf("échec création LaunchAgents: %w", err)
		}
		if err := os.WriteFile(path, []byte(plist), 0644); err != nil {
			return fmt.Errorf("échec écriture plist: %w", err)
		}
	default:
		return errors.New("opération non supportée sur cet OS")
	}
	logger.Info("Application ajoutée au démarrage")
	return nil
}

func removeFromStartup() error {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("reg", "delete", `HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\Run`, "/v", "TempoEDF", "/f")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("échec suppression du démarrage: %w", err)
		}
	case "darwin":
		path := macOSPlistPath()
		if path == "" {
			return errors.New("impossible de trouver le plist")
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("échec suppression plist: %w", err)
		}
	default:
		return errors.New("opération non supportée sur cet OS")
	}
	logger.Info("Application supprimée du démarrage")
	return nil
}
