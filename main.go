package main

import (
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

	"github.com/getlantern/systray"
	"github.com/gen2brain/beeep"
)

const (
	defaultAPIURL = "https://www.api-couleur-tempo.fr/api"
	defaultTimeout = 10 * time.Second
)

var (
	logger        *slog.Logger
	httpClient    = &http.Client{Timeout: defaultTimeout}
	config        = Config{
		APIURL:   defaultAPIURL,
		Timeout:  defaultTimeout,
		CacheTTL: 30 * time.Minute,
	}
	dataMaj     sync.RWMutex
	currentData = &Data{}
	cache       = make(map[string]*cacheEntry)
	cacheMu     sync.RWMutex

	// Icônes chargées dynamiquement en fonction de l'OS
	icnBlanc []byte
	icnRouge []byte
	icnBleu  []byte

	// Windows-specific
	exePath string
	appDir  string
	startupItem *systray.MenuItem
)

// init charge les icônes en fonction de l'OS et détecte le chemin de l'exécutable (Windows)
func init() {
	// Détection du chemin de l'exécutable et du répertoire (pour assets absolus)
	var err error
	exePath, err = os.Executable()
	if err != nil {
		panic(fmt.Sprintf("Erreur détection chemin exécutable: %v", err))
	}
	appDir = filepath.Dir(exePath)

	var blancExt, rougeExt, bleuExt string
	switch runtime.GOOS {
	case "windows":
		blancExt = "assets/white.ico"
		rougeExt = "assets/red.ico"
		bleuExt = "assets/blue.ico"
	//case "darwin": // macOS : utiliser PNG pour meilleur rendu en couleur
	//	blancExt = "assets/icon_white.png"
	//	rougeExt = "assets/icon_red.png"
	//	bleuExt = "assets/icon_blue.png"
	//default: // linux et autres
	//	blancExt = "assets/icon_white.png"
	//	rougeExt = "assets/icon_red.png"
	//	bleuExt = "assets/icon_blue.png"
	}
	icnBlanc = mustAsset(blancExt)
	icnRouge = mustAsset(rougeExt)
	icnBleu = mustAsset(bleuExt)
}

// mustAsset charge un asset binaire depuis les fichiers statiques (chemin absolu)
func mustAsset(relPath string) []byte {
	path := filepath.Join(appDir, relPath)
	data, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("Erreur chargement asset %s (absolu: %s): %v", relPath, path, err)) // Panique pour alerter si fichier manquant
	}
	if len(data) == 0 {
		panic(fmt.Sprintf("Asset %s est vide (absolu: %s)", relPath, path))
	}
	return data
}

// Config contient les configurations de l'application
type Config struct {
	APIURL    string
	Timeout   time.Duration
	CacheTTL  time.Duration
}

// Data stocke les données tempo actuelles
type Data struct {
	CurrentTarif  float64
	TarifLib      string
	TodayColor    string
	TomorrowColor string
	LastUpdated   time.Time
}

// TempoResponse représente la réponse de l'API Tempo
type TempoResponse struct {
	DateJour string `json:"dateJour"`
	CodeJour int    `json:"codeJour"`
	Periode  string `json:"periode"`
	// LibCouleur absent de l'API, donc non utilisé
}

// NowResponse représente la réponse de l'API now
type NowResponse struct {
	ApplicableIn int     `json:"applicableIn"`
	CodeCouleur  int     `json:"codeCouleur"`
	CodeHoraire  int     `json:"codeHoraire"`
	TarifKwh     float64 `json:"tarifKwh"`
	LibTarif     string  `json:"libTarif"`
}

func main() {
	// Initialize logger with structured logging (stdout + fichier pour debug démarrage)
	logFile, err := os.OpenFile(filepath.Join(appDir, "tempo_edf.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		panic(fmt.Sprintf("Erreur ouverture fichier log: %v", err))
	}
	mw := io.MultiWriter(os.Stdout, logFile)
	logger = slog.New(slog.NewTextHandler(mw, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	logger.Info("Tempo EDF démarré", "exePath", exePath, "appDir", appDir)
	systray.Run(onReady, onExit)
}

// isInStartup vérifie si l'application est déjà dans le registre de démarrage Windows
func isInStartup() bool {
	if runtime.GOOS != "windows" || exePath == "" {
		return false
	}
	cmd := exec.Command("reg", "query", `HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\Run`, "/v", "TempoEDF")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

// addToStartup ajoute l'application au démarrage via le registre Windows
func addToStartup() error {
	if runtime.GOOS != "windows" || exePath == "" {
		return errors.New("opération non supportée ou chemin non détecté")
	}
	quotedPath := `"` + exePath + `"`
	cmd := exec.Command("reg", "add", `HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\Run`, "/v", "TempoEDF", "/t", "REG_SZ", "/d", quotedPath, "/f")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("échec ajout au démarrage: %w", err)
	}
	logger.Info("Application ajoutée au démarrage Windows")
	return nil
}

// removeFromStartup supprime l'application du démarrage Windows
func removeFromStartup() error {
	if runtime.GOOS != "windows" {
		return errors.New("opération non supportée")
	}
	cmd := exec.Command("reg", "delete", `HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\Run`, "/v", "TempoEDF", "/f")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("échec suppression du démarrage: %w", err)
	}
	logger.Info("Application supprimée du démarrage Windows")
	return nil
}

func onReady() {
	// Create menu items
	systray.SetTitle("Tempo EDF")
	systray.SetTooltip("Tempo EDF - Couleur du jour")

	// Initialize colors and tarifs
	updateData()
	updateIconBasedOnColor() // Mise à jour icône après données

	// Send initial notification
	sendNotification("Tempo EDF", fmt.Sprintf("Couleur d'aujourd'hui : %s - Tarif : %.3f€/kWh",
		currentData.TodayColor, currentData.CurrentTarif), "")

	// Create menu items with dynamic content
	todayItem := systray.AddMenuItem(fmt.Sprintf("Aujourd'hui : %s", currentData.TodayColor), "Couleur d'aujourd'hui")
	todayItem.SetTooltip(fmt.Sprintf("Couleur d'aujourd'hui : %s", currentData.TodayColor))

	tomorrowItem := systray.AddMenuItem(fmt.Sprintf("Demain : %s", currentData.TomorrowColor), "Couleur de demain")
	tomorrowItem.SetTooltip(fmt.Sprintf("Couleur de demain : %s", currentData.TomorrowColor))

	tarifsItem := systray.AddMenuItem(fmt.Sprintf("Tarif actuel : %.3f€/kWh", currentData.CurrentTarif), "Tarifs Tempo EDF")
	tarifsItem.SetTooltip(fmt.Sprintf("Tarif actuel : %.3f€/kWh - %s", currentData.CurrentTarif, currentData.TarifLib))

	refreshItem := systray.AddMenuItem("Rafraîchir", "Rafraîchir les données")
	refreshItem.SetTooltip("Rafraîchir les données")

	// Option démarrage Windows (uniquement si Windows)
	if runtime.GOOS == "windows" && exePath != "" {
		checked := isInStartup()
		startupItem = systray.AddMenuItemCheckbox("Démarrer avec Windows", "Lancer au démarrage du PC", checked)
		startupItem.SetTooltip("Ajouter/supprimer du démarrage automatique")
	}

	systray.AddSeparator()

	quitItem := systray.AddMenuItem("Quitter", "Quitter l'application")
	quitItem.SetTooltip("Quitter l'application")

	// Handle menu item clicks
	go handleMenuClicks(todayItem, tomorrowItem, tarifsItem, refreshItem, quitItem)

	// Start midnight scheduler
	go scheduleMidnightNotification()

	logger.Info("Interface système prêt")
}

func onExit() {
	logger.Info("Tempo EDF en train de quitter...")
}

func handleMenuClicks(todayItem, tomorrowItem, tarifsItem, refreshItem, quitItem *systray.MenuItem) {
	for {
		select {
		case <-todayItem.ClickedCh:
			sendNotification("Tempo EDF", fmt.Sprintf("Aujourd'hui : %s", currentData.TodayColor), "")
		case <-tomorrowItem.ClickedCh:
			sendNotification("Tempo EDF", fmt.Sprintf("Demain : %s", currentData.TomorrowColor), "")
		case <-tarifsItem.ClickedCh:
			sendNotification("Tempo EDF", fmt.Sprintf("Tarif actuel : %.3f€/kWh - %s", currentData.CurrentTarif, currentData.TarifLib), "")
		case <-refreshItem.ClickedCh:
			updateData()
			updateMenuItems(todayItem, tomorrowItem, tarifsItem)
			updateIconBasedOnColor() // Mise à jour icône après refresh
			sendNotification("Tempo EDF", fmt.Sprintf("Données mises à jour : %s - Tarif : %.3f€/kWh",
				currentData.TodayColor, currentData.CurrentTarif), "")
		case <-quitItem.ClickedCh:
			systray.Quit()
		case <-startupItem.ClickedCh: // Gestion du checkbox Windows
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
	}
}

func updateMenuItems(todayItem, tomorrowItem, tarifsItem *systray.MenuItem) {
	todayItem.SetTitle(fmt.Sprintf("Aujourd'hui : %s", currentData.TodayColor))
	tomorrowItem.SetTitle(fmt.Sprintf("Demain : %s", currentData.TomorrowColor))
	tarifsItem.SetTitle(fmt.Sprintf("Tarif actuel : %.3f€/kWh", currentData.CurrentTarif))
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
	dataMaj.Lock()
	defer dataMaj.Unlock()

	logger.Debug("Récupération de la couleur d'aujourd'hui")

	today, err := fetch[TempoResponse](fmt.Sprintf("%s/jourTempo/today", defaultAPIURL))
	if err != nil {
		logger.Error("Erreur récupération couleur aujourd'hui", "error", err)
		currentData.TodayColor = "ERREUR"
		return
	}

	currentData.TodayColor = codeToColor(today.CodeJour)
	logger.Info("Couleur d'aujourd'hui", "color", currentData.TodayColor)

	logger.Debug("Récupération de la couleur de demain")

	tomorrow, err := fetch[TempoResponse](fmt.Sprintf("%s/jourTempo/tomorrow", defaultAPIURL))
	if err != nil {
		logger.Error("Erreur récupération couleur demain", "error", err)
		currentData.TomorrowColor = "ERREUR"
		return
	}

	currentData.TomorrowColor = codeToColor(tomorrow.CodeJour)
	logger.Info("Couleur de demain", "color", currentData.TomorrowColor)
}

func updateCurrentTarif() {
	dataMaj.Lock()
	defer dataMaj.Unlock()

	logger.Debug("Récupération du tarif actuel")

	now, err := fetch[NowResponse](fmt.Sprintf("%s/now", defaultAPIURL))
	if err != nil {
		logger.Error("Erreur récupération tarif", "error", err)
		currentData.CurrentTarif = 0
		currentData.TarifLib = "Erreur"
		return
	}

	currentData.CurrentTarif = now.TarifKwh
	currentData.TarifLib = now.LibTarif
	logger.Info("Tarif actuel", "tarif", now.TarifKwh, "libelle", now.LibTarif)
}

// cacheEntry représente une entrée de cache avec sa durée de validité
type cacheEntry struct {
	data    []byte
	expires time.Time
}

// fetch utilise le cache si disponible, sinon fait une requête HTTP
func fetch[T any](url string) (T, error) {
	// Check cache first
	cacheMu.RLock()
	entry, exists := cache[url]
	if exists && time.Now().Before(entry.expires) {
		logger.Debug("Cache hit", "url", url)
		data := entry.data
		cacheMu.RUnlock()
		var result T
		if err := json.Unmarshal(data, &result); err != nil {
			logger.Error("Erreur parsing cache", "error", err)
			var zero T
			return zero, err
		}
		return result, nil
	}
	cacheMu.RUnlock()

	logger.Debug("Requête HTTP", "url", url)

	resp, err := httpClient.Get(url)
	if err != nil {
		logger.Error("Erreur HTTP", "url", url, "error", err)
		var zero T
		return zero, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.Error("Erreur statut HTTP", "url", url, "status", resp.StatusCode)
		var zero T
		return zero, errors.New("unexpected status code")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error("Erreur lecture réponse", "error", err)
		var zero T
		return zero, err
	}

	var result T
	if err = json.Unmarshal(body, &result); err != nil {
		logger.Error("Erreur parsing JSON", "error", err)
		var zero T
		return zero, err
	}

	// Cache the result (only if no error)
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

	err := beeep.Notify(title, message, appIcon)
	if err != nil {
		logger.Error("Erreur notification", "error", err)
	} else {
		logger.Info("Notification envoyée", "title", title)
	}
}

func scheduleMidnightNotification() {
	for {
		now := time.Now()
		nextMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
		duration := nextMidnight.Sub(now)
		logger.Debug("Attente jusqu'au prochain minuit", "duration", duration)
		time.Sleep(duration)
		updateData()
		updateIconBasedOnColor()
		sendNotification("Tempo EDF", fmt.Sprintf("Nouveau jour : %s - Tarif : %.3f€/kWh", currentData.TodayColor, currentData.CurrentTarif), "")
	}
}

// updateIconBasedOnColor met à jour l'icône sans relancer l'application
func updateIconBasedOnColor() {
	icon := loadIcon(currentData.TodayColor)
	if len(icon) > 0 {
		systray.SetIcon(icon)
		logger.Info("Icône mise à jour", "color", currentData.TodayColor, "size", len(icon))
	} else {
		logger.Warn("Icône non définie (données vides ou fichier manquant) - Utilisation fallback")
		systray.SetIcon(icnBlanc) // Fallback forcé
	}
}

// loadIcon charge l'icône correspondant à la couleur Tempo actuelle
func loadIcon(colorName string) []byte {
	// Tente d'abord les icônes chargées
	colorKey := strings.ToLower(strings.TrimSpace(colorName))
	switch colorKey {
	case "blanc":
		return icnBlanc
	case "rouge":
		return icnRouge
	case "bleu":
		return icnBleu
	case "inconnu", "erreur":
		return icnBlanc // Fallback explicite pour erreurs
	}

	// Fallback: charge depuis les fichiers (ajusté par OS)
	var ext string
	switch runtime.GOOS {
	case "windows":
		ext = ".ico"
	case "darwin":
		ext = ".png"
	default:
		ext = ".png"
	}
	var iconPaths = []string{
		fmt.Sprintf("assets/%s%s", colorKey, ext),
		fmt.Sprintf("assets/%s%s", strings.Title(colorKey), ext),
		fmt.Sprintf("assets/icon_%s%s", colorKey, ext),
	}

	for _, relPath := range iconPaths {
		path := filepath.Join(appDir, relPath)
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 100 {
			return data
		}
	}

	// Fallback par défaut: icône blanche
	logger.Warn("Aucune icône trouvée pour couleur", "color", colorName)
	return icnBlanc
}