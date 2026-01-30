# Tempo EDF Tray App ⚡

Une application légère pour la barre des tâches (system tray) écrite en **Go**. Elle permet de suivre en temps réel les **couleurs Tempo d'EDF** (Bleu, Blanc, Rouge) et les tarifs actuels directement depuis votre bureau.

---

## 🚀 Fonctionnalités Clés

* **Icône dynamique** : L'icône change de couleur (bleu, blanc, rouge) selon le jour actuel.
* **Menu complet** : Accès rapide à la couleur du jour, de demain et au tarif précis en **€/kWh**.
* **Notifications intelligentes** : Alertes au démarrage, à minuit (changement de jour) et lors d'un rafraîchissement manuel.
* **Démarrage automatique** : Option intégrée pour lancer l'app avec Windows (via le registre HKCU).
* **Optimisation** : Système de cache intégré pour limiter les requêtes vers l'API.

---

## 📋 Prérequis

* **Go** : Version **1.20 ou supérieure**. [Télécharger Go](https://go.dev/dl/).
* **API** : Utilise l'API publique de [api-couleur-tempo.fr](https://api-couleur-tempo.fr).
* **Actifs (Icons)** : Vous devez créer un dossier `assets/` à la racine avec les fichiers suivants :
    * `white.ico`
    * `red.ico`
    * `blue.ico`
    * *Note : Format ICO recommandé (16x16 ou 32x32px).*

---

## 🛠 Installation et Compilation

### Compiler
Clonez votre projet et placez-vous dans le répertoire :
```bash
git clone https://github.com/makertronic/tempo-edf/
go mod tidy
go build
```




