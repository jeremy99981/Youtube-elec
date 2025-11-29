package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

var baseDir string

func main() {
	exePath, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	baseDir = filepath.Dir(exePath)

	if _, err := os.Stat(filepath.Join(baseDir, "youtube-dl.exe")); err != nil {
		log.Printf("Attention: youtube-dl.exe introuvable dans %s\n", baseDir)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/download", downloadHandler)

	srv := &http.Server{
		Addr:    "127.0.0.1:8080",
		Handler: mux,
	}

	go func() {
		time.Sleep(500 * time.Millisecond)
		openBrowser("http://127.0.0.1:8080/")
	}()

	log.Println("Serveur démarré sur http://127.0.0.1:8080/")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, indexHTML)
}

type downloadRequest struct {
	URL string `json:"url"`
}

type downloadResponse struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
	Error  string `json:"error"`
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req downloadRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "JSON invalide", http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		http.Error(w, "URL manquante", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Minute)
	defer cancel()

	ytdlPath := filepath.Join(baseDir, "youtube-dl.exe")

	cmd := exec.CommandContext(
		ctx,
		ytdlPath,
		"-f", "bestvideo[ext=mp4][vcodec!*=av01]+bestaudio[ext=m4a]/best[ext=mp4][vcodec!*=av01]/best[ext=mp4]/best",
		"--merge-output-format", "mp4",
		"-o", "%(title)s.%(ext)s",
		req.URL,
	)
	cmd.Dir = baseDir
	output, err := cmd.CombinedOutput()

	resp := downloadResponse{
		Output: string(output),
	}
	if err != nil {
		resp.OK = false
		resp.Error = err.Error()
	} else {
		resp.OK = true
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

func openBrowser(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	_ = cmd.Start()
}

const indexHTML = `<!doctype html>
<html lang="fr">
<head>
  <meta charset="utf-8">
  <title>youtube-dl GUI</title>
  <style>
    body { font-family: sans-serif; margin: 20px; }
    label, input, button { font-size: 14px; }
    #url { width: 80%; }
    #log { white-space: pre-wrap; border: 1px solid #ccc; padding: 10px; margin-top: 15px; max-height: 300px; overflow: auto; font-family: monospace; font-size: 12px; }
  </style>
</head>
<body>
  <h1>youtube-dl GUI</h1>
  <p>Collez l'URL de la vidéo YouTube puis cliquez sur "Télécharger". Les fichiers seront enregistrés dans le même dossier que ce programme.</p>
  <label for="url">URL de la vidéo :</label><br>
  <input type="text" id="url" placeholder="https://www.youtube.com/watch?v=..." /><br><br>
  <button id="downloadBtn">Télécharger</button>
  <div id="status"></div>
  <pre id="log"></pre>
<script>
  document.getElementById('downloadBtn').addEventListener('click', function () {
    var url = document.getElementById('url').value.trim();
    var status = document.getElementById('status');
    var log = document.getElementById('log');
    if (!url) {
      status.textContent = 'Veuillez entrer une URL.';
      return;
    }
    status.textContent = 'Téléchargement en cours...';
    log.textContent = '';
    fetch('/download', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url: url })
    }).then(function (res) {
      return res.json();
    }).then(function (data) {
      if (data.ok) {
        status.textContent = 'Téléchargement terminé.';
      } else {
        status.textContent = 'Erreur lors du téléchargement.';
      }
      log.textContent = (data.output || '') + (data.error ? ('\nErreur : ' + data.error) : '');
    }).catch(function (err) {
      status.textContent = 'Erreur réseau.';
      log.textContent = String(err);
    });
  });
</script>
</body>
</html>`
