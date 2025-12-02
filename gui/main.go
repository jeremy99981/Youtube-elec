package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var baseDir string
var ytdlPath string

var (
	jobs                sync.Map
	downloadProgressRe = regexp.MustCompile(`\[download\]\s+(\d+(?:\.\d+)?)%`)
)

func main() {
	exePath, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	baseDir = filepath.Dir(exePath)
	ytdlPath = filepath.Join(baseDir, "youtube-dl.exe")

	if _, err := os.Stat(ytdlPath); err != nil {
		if wd, wdErr := os.Getwd(); wdErr == nil {
			alt := filepath.Join(wd, "youtube-dl.exe")
			if _, altErr := os.Stat(alt); altErr == nil {
				log.Printf("youtube-dl.exe introuvable dans %s, utilisation du répertoire de travail %s\n", baseDir, wd)
				baseDir = wd
				ytdlPath = alt
			}
		}
		if _, err2 := os.Stat(ytdlPath); err2 != nil {
			log.Printf("Attention: youtube-dl.exe introuvable dans %s\n", baseDir)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/download", downloadHandler)
	mux.HandleFunc("/status", statusHandler)

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
	URL  string `json:"url"`
	Mode string `json:"mode"`
}

type downloadResponse struct {
	OK    bool   `json:"ok"`
	ID    string `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
}

type statusResponse struct {
	OK  bool       `json:"ok"`
	Job *jobStatus `json:"job,omitempty"`
	Error string   `json:"error,omitempty"`
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

	mode := normalizeMode(req.Mode)
	cleanURL := normalizeVideoURL(req.URL)
	job := newJob(mode)
	jobs.Store(job.snapshot().ID, job)

	job.appendLog(fmt.Sprintf("URL normalisée: %s", cleanURL))

	// Utilise un contexte de fond pour éviter l'annulation immédiate une fois la requête HTTP servie.
	go startDownload(context.Background(), job, cleanURL)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(downloadResponse{OK: true, ID: job.snapshot().ID})
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	jobID := r.URL.Query().Get("id")
	if jobID == "" {
		http.Error(w, "id manquant", http.StatusBadRequest)
		return
	}
	value, ok := jobs.Load(jobID)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(statusResponse{OK: false, Error: "téléchargement introuvable"})
		return
	}
	job := value.(*job)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	status := job.snapshot()
	_ = json.NewEncoder(w).Encode(statusResponse{OK: true, Job: &status})
}

func openBrowser(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	_ = cmd.Start()
}

type job struct {
	mu     sync.RWMutex
	state  jobStatus
}

type jobStatus struct {
	ID            string     `json:"id"`
	Mode          string     `json:"mode"`
	Status        string     `json:"status"`
	DownloadPct   float64    `json:"downloadPct"`
	ConversionPct float64    `json:"conversionPct"`
	Message       string     `json:"message"`
	Log           string     `json:"log"`
	Error         string     `json:"error,omitempty"`
	Finished      bool       `json:"finished"`
	StartedAt     time.Time  `json:"startedAt"`
	CompletedAt   *time.Time `json:"completedAt,omitempty"`
}

func newJob(mode string) *job {
	return &job{
		state: jobStatus{
			ID:            newJobID(),
			Mode:          mode,
			Status:        "préparation",
			DownloadPct:   0,
			ConversionPct: -1,
			StartedAt:     time.Now(),
		},
	}
}

func (j *job) snapshot() jobStatus {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.state
}

func (j *job) update(fn func(*jobStatus)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	fn(&j.state)
}

func (j *job) appendLog(line string) {
	j.update(func(s *jobStatus) {
		if s.Log != "" {
			s.Log += "\n"
		}
		s.Log += line
		s.Message = line
	})
}

func startDownload(parentCtx context.Context, job *job, url string) {
	ctx, cancel := context.WithTimeout(parentCtx, 60*time.Minute)
	defer cancel()

	args := []string{"--newline", "-o", "%(title)s.%(ext)s"}
	switch job.snapshot().Mode {
	case "audio":
		args = append(args,
			"-f", "bestaudio/best",
			"--extract-audio",
			"--audio-format", "mp3",
			"--audio-quality", "0",
		)
	default:
		args = append(args,
			"-f", "bestvideo[ext=mp4][vcodec!*=av01]+bestaudio[ext=m4a]/best[ext=mp4][vcodec!*=av01]/best[ext=mp4]/best",
			"--merge-output-format", "mp4",
		)
	}
	args = append(args, url)

	cmd := exec.CommandContext(ctx, ytdlPath, args...)
	cmd.Dir = baseDir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		jobFailed(job, fmt.Errorf("stdout pipe: %w", err))
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		jobFailed(job, fmt.Errorf("stderr pipe: %w", err))
		return
	}

	if err := cmd.Start(); err != nil {
		jobFailed(job, err)
		return
	}

	reader := bufio.NewReader(io.MultiReader(stdout, stderr))
	go streamLines(job, reader)

	if err := cmd.Wait(); err != nil {
		jobFailed(job, err)
		return
	}
	completion := time.Now()
	job.update(func(s *jobStatus) {
		s.Status = "terminé"
		s.DownloadPct = 100
		if s.ConversionPct < 0 {
			s.ConversionPct = 100
		} else {
			s.ConversionPct = 100
		}
		s.Finished = true
		s.CompletedAt = &completion
	})
}

func streamLines(job *job, reader *bufio.Reader) {
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			clean := strings.TrimSpace(strings.ReplaceAll(line, "\r", ""))
			if clean != "" {
				job.appendLog(clean)
				interpretLine(job, clean)
			}
		}
		if err != nil {
			if err != io.EOF {
				job.appendLog(fmt.Sprintf("flux interrompu: %v", err))
			}
			return
		}
	}
}

func interpretLine(job *job, line string) {
	if matches := downloadProgressRe.FindStringSubmatch(line); len(matches) == 2 {
		if pct, err := strconv.ParseFloat(matches[1], 64); err == nil {
			job.update(func(s *jobStatus) {
				s.Status = "téléchargement"
				s.DownloadPct = pct
			})
		}
		return
	}
	if strings.Contains(line, "[ffmpeg]") || strings.Contains(line, "[Merger]") || strings.Contains(strings.ToLower(line), "conversion") {
		job.update(func(s *jobStatus) {
			s.Status = "conversion"
			if s.ConversionPct < 0 {
				s.ConversionPct = 0
			}
		})
	}
}

func jobFailed(job *job, err error) {
	completion := time.Now()
	job.appendLog(fmt.Sprintf("Erreur: %v", err))
	job.update(func(s *jobStatus) {
		s.Status = "erreur"
		s.Error = err.Error()
		s.Message = err.Error()
		s.Finished = true
		s.CompletedAt = &completion
	})
}

func normalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "audio", "son", "music":
		return "audio"
	default:
		return "video"
	}
}

func newJobID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func normalizeVideoURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return trimmed
	}
	host := strings.ToLower(u.Host)
	path := strings.Trim(u.Path, "/")
	makeWatch := func(id string) string {
		id = strings.TrimSpace(id)
		if id == "" {
			return trimmed
		}
		return fmt.Sprintf("https://www.youtube.com/watch?v=%s", id)
	}
	if host == "youtu.be" || host == "www.youtu.be" {
		parts := strings.Split(path, "/")
		if len(parts) > 0 {
			return makeWatch(parts[0])
		}
	}
	if strings.Contains(host, "youtube.com") {
		if id := u.Query().Get("v"); id != "" {
			return makeWatch(id)
		}
		if strings.HasPrefix(path, "shorts/") {
			id := strings.TrimPrefix(path, "shorts/")
			id = strings.Split(id, "/")[0]
			return makeWatch(id)
		}
	}
	return trimmed
}

const indexHTML = `<!doctype html>
<html lang="fr">
<head>
  <meta charset="utf-8">
  <title>Youtube Elec Downloader</title>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <style>
    :root {
      --primary: #ff4d5a;
      --primary-dark: #b81441;
      --bg: #0f172a;
      --card: rgba(15, 23, 42, 0.8);
      --text: #f8fafc;
      --muted: #94a3b8;
      --border: rgba(148, 163, 184, 0.2);
    }
    * { box-sizing: border-box; }
    body {
      font-family: "Inter", "Segoe UI", system-ui, -apple-system, sans-serif;
      margin: 0;
      min-height: 100vh;
      background: radial-gradient(circle at top, #1e2a47, #050914 60%);
      color: var(--text);
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 32px;
    }
    .app {
      width: min(960px, 100%);
      display: grid;
      gap: 24px;
    }
    .card {
      background: var(--card);
      border: 1px solid var(--border);
      border-radius: 24px;
      padding: 32px;
      box-shadow: 0 20px 60px rgba(0, 0, 0, 0.35);
      backdrop-filter: blur(16px);
    }
    h1 {
      margin: 0 0 8px;
      font-size: clamp(1.6rem, 3vw, 2.4rem);
    }
    p.description {
      margin: 0;
      color: var(--muted);
    }
    label {
      display: block;
      font-weight: 600;
      margin-bottom: 8px;
    }
    input[type="text"] {
      width: 100%;
      padding: 16px;
      border-radius: 16px;
      border: 1px solid var(--border);
      background: rgba(15, 23, 42, 0.6);
      color: var(--text);
      font-size: 1rem;
      transition: border 0.3s, box-shadow 0.3s;
    }
    input[type="text"]:focus {
      outline: none;
      border-color: var(--primary);
      box-shadow: 0 0 0 3px rgba(255, 77, 90, 0.25);
    }
    .mode-selector {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
      gap: 16px;
      margin-top: 12px;
    }
    .mode-card {
      position: relative;
      border: 1px solid var(--border);
      border-radius: 18px;
      padding: 20px;
      cursor: pointer;
      transition: border 0.3s, transform 0.3s, background 0.3s;
      background: rgba(255, 255, 255, 0.01);
    }
    .mode-card:hover { transform: translateY(-2px); }
    .mode-card input {
      position: absolute;
      inset: 0;
      opacity: 0;
      cursor: pointer;
    }
    .mode-card.active {
      border-color: var(--primary);
      background: linear-gradient(135deg, rgba(255, 77, 90, 0.15), rgba(255, 77, 90, 0.05));
      box-shadow: 0 10px 25px rgba(255, 77, 90, 0.25);
    }
    .mode-card h3 {
      margin: 0 0 4px;
      font-size: 1.1rem;
    }
    .mode-card span {
      color: var(--muted);
      font-size: 0.9rem;
    }
    button.primary {
      width: 100%;
      padding: 16px;
      border-radius: 18px;
      border: none;
      background: linear-gradient(135deg, #ff4d5a, #ff8055);
      color: white;
      font-weight: 700;
      font-size: 1.1rem;
      cursor: pointer;
      transition: transform 0.2s, box-shadow 0.2s;
    }
    button.primary:disabled {
      opacity: 0.6;
      cursor: not-allowed;
      box-shadow: none;
    }
    button.primary:not(:disabled):hover {
      transform: translateY(-1px);
      box-shadow: 0 15px 30px rgba(255, 77, 90, 0.35);
    }
    .status-bar {
      display: flex;
      align-items: center;
      gap: 12px;
      margin-top: 12px;
      font-weight: 600;
    }
    .badge {
      padding: 6px 12px;
      border-radius: 999px;
      font-size: 0.85rem;
      text-transform: capitalize;
      background: rgba(148, 163, 184, 0.2);
    }
    .badge.success { background: rgba(34, 197, 94, 0.18); }
    .badge.error { background: rgba(239, 68, 68, 0.18); }
    .badge.progress { background: rgba(59, 130, 246, 0.18); }
    .progress-card {
      border-top: 1px solid var(--border);
      padding-top: 24px;
      margin-top: 24px;
    }
    .progress-item { margin-bottom: 24px; }
    .progress-label {
      display: flex;
      justify-content: space-between;
      font-size: 0.9rem;
      color: var(--muted);
      margin-bottom: 8px;
    }
    .progress-track {
      height: 14px;
      border-radius: 999px;
      background: rgba(148, 163, 184, 0.2);
      overflow: hidden;
      position: relative;
    }
    .progress-fill {
      position: absolute;
      inset: 0;
      border-radius: inherit;
      background: linear-gradient(90deg, #38bdf8, #6366f1);
      width: 0;
      transition: width 0.4s ease;
    }
    .progress-fill.indeterminate {
      background: linear-gradient(120deg, rgba(99, 102, 241, 0.2), rgba(99, 102, 241, 0.6), rgba(99, 102, 241, 0.2));
      animation: shimmer 1.2s infinite;
    }
    @keyframes shimmer {
      0% { transform: translateX(-100%); }
      50% { transform: translateX(10%); }
      100% { transform: translateX(100%); }
    }
    pre {
      background: rgba(2, 6, 23, 0.7);
      border: 1px solid rgba(15, 23, 42, 0.6);
      border-radius: 18px;
      padding: 20px;
      font-family: "JetBrains Mono", "Fira Code", monospace;
      font-size: 0.85rem;
      color: #a5b4fc;
      max-height: 280px;
      overflow: auto;
      white-space: pre-wrap;
    }
    @media (max-width: 600px) {
      body { padding: 16px; }
      .card { padding: 20px; }
    }
  </style>
</head>
<body>
  <main class="app">
    <section class="card">
      <header>
        <h1>Youtube Elec Downloader</h1>
        <p class="description">Téléchargements de vidéos ou musiques YouTube sur PC ElectroDepot en un clic.</p>
      </header>
      <div class="form">
        <label for="url">URL YouTube</label>
        <input type="text" id="url" placeholder="https://www.youtube.com/watch?v=..." autocomplete="off" />
        <div class="mode-selector" id="modeSelector">
          <label class="mode-card active">
            <input type="radio" name="mode" value="video" checked />
            <h3>Vidéo + Audio</h3>
            <span>MP4 optimisé (jusqu'à 1080p) avec fusion automatique.</span>
          </label>
          <label class="mode-card">
            <input type="radio" name="mode" value="audio" />
            <h3>Audio seul</h3>
            <span>Extraction MP3 haute qualité idéale pour les podcasts & musique.</span>
          </label>
        </div>
        <button class="primary" id="downloadBtn">Lancer le téléchargement</button>
        <div class="status-bar">
          <span>Statut :</span>
          <span class="badge" id="statusBadge">En attente</span>
          <span id="statusMessage"></span>
        </div>
      </div>
      <div class="progress-card">
        <div class="progress-item">
          <div class="progress-label">
            <span>Téléchargement</span>
            <span id="downloadValue">0%</span>
          </div>
          <div class="progress-track">
            <div class="progress-fill" id="downloadFill"></div>
          </div>
        </div>
        <div class="progress-item">
          <div class="progress-label">
            <span>Conversion / Fusion</span>
            <span id="convertValue">0%</span>
          </div>
          <div class="progress-track">
            <div class="progress-fill indeterminate" id="convertFill"></div>
          </div>
        </div>
      </div>
    </section>
    <section class="card">
      <header>
        <h2>Journal en direct</h2>
        <p class="description">Suivez les étapes détaillées du téléchargement et de la conversion.</p>
      </header>
      <pre id="log">En attente d'un téléchargement...</pre>
    </section>
  </main>
  <script>
    const downloadBtn = document.getElementById('downloadBtn');
    const statusBadge = document.getElementById('statusBadge');
    const statusMessage = document.getElementById('statusMessage');
    const downloadFill = document.getElementById('downloadFill');
    const downloadValue = document.getElementById('downloadValue');
    const convertFill = document.getElementById('convertFill');
    const convertValue = document.getElementById('convertValue');
    const logEl = document.getElementById('log');
    const urlInput = document.getElementById('url');
    const modeCards = document.querySelectorAll('.mode-card');

    let activeJobId = null;
    let poller = null;

    modeCards.forEach(card => {
      card.addEventListener('click', () => {
        modeCards.forEach(c => c.classList.remove('active'));
        card.classList.add('active');
        card.querySelector('input').checked = true;
      });
    });

    function setBadge(status, tone = 'progress') {
      statusBadge.className = 'badge ' + tone;
      statusBadge.textContent = status;
    }

    function resetUI() {
      setBadge('En attente');
      statusMessage.textContent = '';
      downloadFill.style.width = '0%';
      downloadValue.textContent = '0%';
      convertFill.style.width = '0%';
      convertValue.textContent = '0%';
      convertFill.classList.add('indeterminate');
      logEl.textContent = "En attente d'un téléchargement...";
    }

    function updateProgress(job) {
      const pct = Math.min(100, Math.max(0, job.downloadPct || 0));
      downloadFill.style.width = pct + '%';
      downloadValue.textContent = pct.toFixed(0) + '%';

      if (job.conversionPct === -1) {
        convertFill.classList.add('indeterminate');
        convertFill.style.width = '35%';
        convertValue.textContent = '...';
      } else {
        convertFill.classList.remove('indeterminate');
        const cPct = Math.min(100, Math.max(0, job.conversionPct || 0));
        convertFill.style.width = cPct + '%';
        convertValue.textContent = cPct.toFixed(0) + '%';
      }

      logEl.textContent = job.log || 'Logs en cours...';
      statusMessage.textContent = job.message || '';

      if (job.status === 'terminé') {
        setBadge('Terminé', 'success');
      } else if (job.status === 'erreur') {
        setBadge('Erreur', 'error');
      } else {
        setBadge(job.status, 'progress');
      }
    }

    function stopPolling() {
      if (poller) {
        clearInterval(poller);
        poller = null;
      }
      activeJobId = null;
      downloadBtn.disabled = false;
    }

    async function pollStatus() {
      if (!activeJobId) return;
      try {
        const res = await fetch('/status?id=' + activeJobId);
        if (!res.ok) throw new Error('Statut indisponible');
        const data = await res.json();
        if (!data.ok || !data.job) throw new Error(data.error || 'Réponse invalide');
        updateProgress(data.job);
        if (data.job.finished) {
          stopPolling();
        }
      } catch (err) {
        console.error(err);
        statusMessage.textContent = 'Impossible de rafraîchir le statut';
      }
    }

    downloadBtn.addEventListener('click', async () => {
      const url = urlInput.value.trim();
      const mode = document.querySelector('input[name="mode"]:checked').value;

      if (!url) {
        statusMessage.textContent = 'Veuillez entrer une URL valide.';
        setBadge('URL manquante', 'error');
        return;
      }

      downloadBtn.disabled = true;
      setBadge('Initialisation');
      statusMessage.textContent = 'Préparation du téléchargement...';
      downloadFill.style.width = '0%';
      convertFill.style.width = '25%';
      convertFill.classList.add('indeterminate');
      logEl.textContent = 'Connexion au service...';

      try {
        const res = await fetch('/download', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ url, mode })
        });
        if (!res.ok) throw new Error('Téléchargement impossible');
        const data = await res.json();
        if (!data.ok || !data.id) throw new Error(data.error || 'Réponse invalide');
        activeJobId = data.id;
        pollStatus();
        poller = setInterval(pollStatus, 1500);
      } catch (err) {
        console.error(err);
        setBadge('Erreur', 'error');
        statusMessage.textContent = err.message;
        downloadBtn.disabled = false;
      }
    });

    resetUI();
  </script>
</body>
</html>`
