(() => {
  "use strict";

  const STORAGE = Object.freeze({
    books: "audiobook-ui.books.v2",
    lastJobID: "audiobook-ui.last-job-id.v2",
    settings: "audiobook-ui.generation-settings.v2",
  });
  const POLL_MS = 2500;
  const JOBS_PAGE_SIZE = 8;
  const TERMINAL_JOB_STATUSES = new Set([
    "completed",
    "completed_with_warnings",
    "failed",
  ]);
  const EDITABLE_STATUSES = new Set(["ready", "warning", "failed"]);
  const RETRYABLE_STATUSES = new Set(["warning", "failed"]);
  const STATUS_LABELS = Object.freeze({
    queued: "В очереди",
    running: "Озвучивается",
    completed: "Готово",
    completed_with_warnings: "Нужна проверка",
    failed: "Завершено с ошибками",
    pending: "Ожидает",
    generating: "Озвучивается",
    ready: "Готов",
    warning: "Проверить",
    failed_fragment: "Ошибка",
  });

  const $ = (id) => document.getElementById(id);
  const clear = (node) => {
    while (node && node.firstChild) node.removeChild(node.firstChild);
  };
  const text = (tag, value, className = "") => {
    const node = document.createElement(tag);
    node.textContent = value;
    if (className) node.className = className;
    return node;
  };
  const button = (label, className, handler) => {
    const node = document.createElement("button");
    node.type = "button";
    node.className = className;
    node.textContent = label;
    node.addEventListener("click", handler);
    return node;
  };
  const formatDuration = (milliseconds) => {
    const totalSeconds = Math.max(0, Math.round(Number(milliseconds || 0) / 1000));
    const minutes = Math.floor(totalSeconds / 60);
    const seconds = totalSeconds % 60;
    return `${minutes}:${String(seconds).padStart(2, "0")}`;
  };
  const readJSON = (key, fallback) => {
    try {
      const value = window.localStorage.getItem(key);
      return value ? JSON.parse(value) : fallback;
    } catch (_) {
      return fallback;
    }
  };
  const writeJSON = (key, value) => {
    try {
      window.localStorage.setItem(key, JSON.stringify(value));
    } catch (_) {
      // The application remains usable when storage is disabled.
    }
  };

  class APIError extends Error {
    constructor(status, code, message) {
      super(message || `HTTP ${status}`);
      this.name = "APIError";
      this.status = status;
      this.code = code || "HTTP_ERROR";
    }
  }

  class APIClient {
    async request(path, options = {}) {
      const headers = new Headers(options.headers || {});
      if (!headers.has("Accept")) headers.set("Accept", "application/json");
      let response;
      try {
        response = await fetch(path, {
          method: options.method || "GET",
          body: options.body,
          headers,
          credentials: "same-origin",
        });
      } catch (_) {
        throw new APIError(0, "NETWORK_ERROR", "Go API недоступен.");
      }
      if (!response.ok) {
        let problem = null;
        try {
          problem = await response.json();
        } catch (_) {
          // A reverse proxy may return a non-JSON error.
        }
        throw new APIError(
          response.status,
          problem && problem.code,
          problem && problem.error ? problem.error : `Запрос завершился с кодом ${response.status}`,
        );
      }
      if (response.status === 204) return null;
      const contentType = response.headers.get("Content-Type") || "";
      return contentType.includes("application/json") ? response.json() : response;
    }
  }

  class AudiobookApp {
    constructor() {
      this.api = new APIClient();
      this.books = readJSON(STORAGE.books, []);
      this.voices = [];
      this.currentJob = null;
      this.currentCatalog = null;
      this.jobPollTimer = null;
      this.jobsPollTimer = null;
      this.jobsPage = 0;
      this.jobsTotal = 0;
      this.selectedFragments = new Set();
      this.busyOperations = new Set();
    }

    async init() {
      this.bindEvents();
      this.restoreSettings();
      this.renderBooks();
      this.updateSelectionSummary();
      this.updateReferenceCount();
      this.configureReviewCopy();
      await Promise.allSettled([
        this.checkHealth(),
        this.loadVoices(),
        this.refreshJobs(),
      ]);
      const lastJobID = window.localStorage.getItem(STORAGE.lastJobID);
      if (lastJobID) await this.openJob(lastJobID, false);
      this.jobsPollTimer = window.setInterval(() => this.refreshJobs(false), 5000);
    }

    configureReviewCopy() {
      const title = $("warnings-title");
      const description = $("warnings-description");
      if (title) title.textContent = "Фрагменты книги";
      if (description) {
        description.textContent =
          "Фрагменты появляются по мере озвучивания. Готовые можно прослушать, " +
          "а готовые, предупреждённые и ошибочные — исправить и автоматически переозвучить.";
      }
      const chapterTitle = $("chapter-downloads-title");
      if (chapterTitle) chapterTitle.textContent = "Главы FLAC";
    }

    bindEvents() {
      $("book-upload-form")?.addEventListener("submit", (event) => {
        event.preventDefault();
        this.uploadBook(event.currentTarget);
      });
      $("voice-upload-form")?.addEventListener("submit", (event) => {
        event.preventDefault();
        this.uploadVoice(event.currentTarget);
      });
      $("book-select")?.addEventListener("change", () => this.updateSelectionSummary());
      $("voice-select")?.addEventListener("change", () => this.updateSelectionSummary());
      $("book-file")?.addEventListener("change", (event) => {
        const file = event.target.files && event.target.files[0];
        if ($("book-file-label")) $("book-file-label").textContent = file ? file.name : "До 50 МБ, без ZIP-сжатия";
      });
      $("voice-file")?.addEventListener("change", (event) => {
        const file = event.target.files && event.target.files[0];
        if ($("voice-file-label")) $("voice-file-label").textContent = file ? file.name : "До 5 МБ";
      });
      $("reference-text")?.addEventListener("input", () => this.updateReferenceCount());
      $("refresh-voices-button")?.addEventListener("click", () => this.loadVoices());
      $("generate-button")?.addEventListener("click", () => this.generate());
      $("generation-settings-reset")?.addEventListener("click", () => this.resetSettings());
      document.querySelectorAll("#generation-settings input").forEach((input) => {
        input.addEventListener("change", () => this.persistSettings());
      });
      $("job-resume-form")?.addEventListener("submit", (event) => {
        event.preventDefault();
        const id = $("job-id-input")?.value.trim();
        if (id) this.openJob(id, true);
      });
      $("copy-job-id-button")?.addEventListener("click", () => this.copyJobID());
      $("jobs-refresh-button")?.addEventListener("click", () => this.refreshJobs(true));
      $("jobs-status-filter")?.addEventListener("change", () => {
        this.jobsPage = 0;
        this.refreshJobs(true);
      });
      $("jobs-prev-button")?.addEventListener("click", () => {
        if (this.jobsPage > 0) {
          this.jobsPage -= 1;
          this.refreshJobs(true);
        }
      });
      $("jobs-next-button")?.addEventListener("click", () => {
        if ((this.jobsPage + 1) * JOBS_PAGE_SIZE < this.jobsTotal) {
          this.jobsPage += 1;
          this.refreshJobs(true);
        }
      });
      $("retry-selected-button")?.addEventListener("click", () => this.retrySelected());
      $("retry-all-button")?.addEventListener("click", () => this.retryAll());
      $("rewrite-model-status") &&
        ($("rewrite-model-status").textContent = "AI-переписывание осталось доступно через API; ручная правка переозвучивается сразу.");
    }

    async checkHealth() {
      try {
        await this.api.request("/healthz");
        $("health-indicator")?.classList.remove("health-indicator--checking");
        $("health-indicator")?.classList.add("health-indicator--ok");
        if ($("health-label")) $("health-label").textContent = "API готов";
      } catch (error) {
        $("health-indicator")?.classList.remove("health-indicator--checking");
        $("health-indicator")?.classList.add("health-indicator--error");
        if ($("health-label")) $("health-label").textContent = "API недоступен";
        throw error;
      }
    }

    async uploadBook(form) {
      if (this.busyOperations.has("book-upload")) return;
      this.busyOperations.add("book-upload");
      this.setButtonBusy("book-upload-button", true, "Разбираем FB2…");
      try {
        const book = await this.api.request("/v1/book", {
          method: "POST",
          body: new FormData(form),
        });
        this.books = [book, ...this.books.filter((item) => item.id !== book.id)].slice(0, 50);
        writeJSON(STORAGE.books, this.books);
        this.renderBooks(book.id);
        this.toast(`Книга «${book.title || "Без названия"}» загружена.`, "success");
      } catch (error) {
        this.toast(error.message, "error");
      } finally {
        this.busyOperations.delete("book-upload");
        this.setButtonBusy("book-upload-button", false, "Загрузить и разобрать");
      }
    }

    async uploadVoice(form) {
      if (this.busyOperations.has("voice-upload")) return;
      this.busyOperations.add("voice-upload");
      this.setButtonBusy("voice-upload-button", true, "Сохраняем голос…");
      try {
        const voice = await this.api.request("/v1/voice", {
          method: "POST",
          body: new FormData(form),
        });
        await this.loadVoices(voice.id);
        this.toast(`Голос «${voice.name}» создан.`, "success");
      } catch (error) {
        this.toast(error.message, "error");
      } finally {
        this.busyOperations.delete("voice-upload");
        this.setButtonBusy("voice-upload-button", false, "Создать голос");
      }
    }

    renderBooks(selectedID = "") {
      const select = $("book-select");
      if (!select) return;
      const previous = selectedID || select.value;
      clear(select);
      if (!this.books.length) {
        const option = document.createElement("option");
        option.value = "";
        option.textContent = "Сначала загрузите книгу";
        select.appendChild(option);
      } else {
        for (const book of this.books) {
          const option = document.createElement("option");
          option.value = book.id;
          option.textContent = `${book.title || "Без названия"} · ${book.fragments_count} фрагм.`;
          select.appendChild(option);
        }
        if (this.books.some((book) => book.id === previous)) select.value = previous;
      }
      this.updateSelectionSummary();
    }

    async loadVoices(selectedID = "") {
      try {
        const payload = await this.api.request("/v1/voices");
        this.voices = payload.voices || [];
        const select = $("voice-select");
        if (!select) return;
        const previous = selectedID || select.value;
        clear(select);
        if (!this.voices.length) {
          const option = document.createElement("option");
          option.value = "";
          option.textContent = "Сначала создайте голос";
          select.appendChild(option);
        } else {
          for (const voice of this.voices) {
            const option = document.createElement("option");
            option.value = voice.id;
            option.textContent = voice.name;
            select.appendChild(option);
          }
          if (this.voices.some((voice) => voice.id === previous)) select.value = previous;
        }
        this.renderVoices();
        this.updateSelectionSummary();
      } catch (error) {
        this.toast(error.message, "error");
      }
    }

    renderVoices() {
      const list = $("voice-list");
      if (!list) return;
      clear(list);
      for (const voice of this.voices.slice(0, 5)) {
        const item = text("div", `${voice.name} · ${voice.format.toUpperCase()}`, "voice-card");
        list.appendChild(item);
      }
    }

    updateSelectionSummary() {
      const bookID = $("book-select")?.value || "";
      const voiceID = $("voice-select")?.value || "";
      const book = this.books.find((item) => item.id === bookID);
      const voice = this.voices.find((item) => item.id === voiceID);
      if ($("selected-book-label")) $("selected-book-label").textContent = book ? book.title : "не выбрана";
      if ($("selected-voice-label")) $("selected-voice-label").textContent = voice ? voice.name : "не выбран";
      if ($("book-summary")) {
        $("book-summary").textContent = book
          ? `${(book.authors || []).join(", ") || "Автор не указан"} · ${book.chapters_count} гл. · ${book.fragments_count} фрагм.`
          : "Здесь появятся название, автор и количество фрагментов.";
      }
      if ($("generate-button")) $("generate-button").disabled = !(bookID && voiceID);
    }

    collectGenerationSettings() {
      const number = (id) => Number($(id)?.value);
      return {
        omnivoice: {
          num_steps: number("tts-num-steps"),
          guidance_scale: number("tts-guidance-scale"),
          speed: number("tts-speed"),
          normalize_text: Boolean($("tts-normalize-text")?.checked),
          denoise: Boolean($("tts-denoise")?.checked),
          t_shift: number("tts-t-shift"),
          layer_penalty_factor: number("tts-layer-penalty"),
          position_temperature: number("tts-position-temperature"),
          class_temperature: number("tts-class-temperature"),
          preprocess_prompt: Boolean($("tts-preprocess-prompt")?.checked),
          postprocess_output: Boolean($("tts-postprocess-output")?.checked),
          audio_chunk_duration: number("tts-chunk-duration"),
          audio_chunk_threshold: number("tts-chunk-threshold"),
          pad_duration: number("tts-pad-duration"),
          fade_duration: number("tts-fade-duration"),
        },
        whisper: {
          beam_size: number("stt-beam-size"),
          patience: number("stt-patience"),
          temperature: number("stt-temperature"),
          vad_filter: Boolean($("stt-vad-filter")?.checked),
          word_timestamps: Boolean($("stt-word-timestamps")?.checked),
        },
        automatic_warning_retries: number("warning-retries"),
      };
    }

    persistSettings() {
      writeJSON(STORAGE.settings, this.collectGenerationSettings());
    }

    restoreSettings() {
      const settings = readJSON(STORAGE.settings, null);
      if (!settings) return;
      const assign = (id, value) => {
        const input = $(id);
        if (!input || value === undefined) return;
        if (input.type === "checkbox") input.checked = Boolean(value);
        else input.value = String(value);
      };
      const omni = settings.omnivoice || {};
      const whisper = settings.whisper || {};
      assign("tts-num-steps", omni.num_steps);
      assign("tts-guidance-scale", omni.guidance_scale);
      assign("tts-speed", omni.speed);
      assign("tts-normalize-text", omni.normalize_text);
      assign("tts-denoise", omni.denoise);
      assign("tts-t-shift", omni.t_shift);
      assign("tts-layer-penalty", omni.layer_penalty_factor);
      assign("tts-position-temperature", omni.position_temperature);
      assign("tts-class-temperature", omni.class_temperature);
      assign("tts-preprocess-prompt", omni.preprocess_prompt);
      assign("tts-postprocess-output", omni.postprocess_output);
      assign("tts-chunk-duration", omni.audio_chunk_duration);
      assign("tts-chunk-threshold", omni.audio_chunk_threshold);
      assign("tts-pad-duration", omni.pad_duration);
      assign("tts-fade-duration", omni.fade_duration);
      assign("stt-beam-size", whisper.beam_size);
      assign("stt-patience", whisper.patience);
      assign("stt-temperature", whisper.temperature);
      assign("stt-vad-filter", whisper.vad_filter);
      assign("stt-word-timestamps", whisper.word_timestamps);
      assign("warning-retries", settings.automatic_warning_retries);
    }

    resetSettings() {
      window.localStorage.removeItem(STORAGE.settings);
      window.location.reload();
    }

    async generate() {
      const bookID = $("book-select")?.value || "";
      const voiceID = $("voice-select")?.value || "";
      if (!bookID || !voiceID || this.busyOperations.has("generate")) return;
      const invalid = Array.from(document.querySelectorAll("#generation-settings input"))
        .find((field) => !field.checkValidity());
      if (invalid) {
        invalid.reportValidity();
        return;
      }
      this.busyOperations.add("generate");
      this.setButtonBusy("generate-button", true, "Ставим в очередь…");
      try {
        const settings = this.collectGenerationSettings();
        this.persistSettings();
        const payload = await this.api.request(
          `/v1/generate/book/${encodeURIComponent(bookID)}/voice/${encodeURIComponent(voiceID)}`,
          {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(settings),
          },
        );
        await this.openJob(payload.job_id, true);
        this.toast("Генерация запущена. Фрагменты появятся по мере готовности.", "success");
      } catch (error) {
        this.toast(error.message, "error");
      } finally {
        this.busyOperations.delete("generate");
        this.setButtonBusy("generate-button", false, "Начать генерацию");
        this.updateSelectionSummary();
      }
    }

    async openJob(jobID, notify = false) {
      if (!jobID) return;
      try {
        const job = await this.api.request(`/v1/job/${encodeURIComponent(jobID)}`);
        this.currentJob = job;
        window.localStorage.setItem(STORAGE.lastJobID, jobID);
        if ($("job-id-input")) $("job-id-input").value = jobID;
        this.renderJob(job);
        await this.loadFragmentCatalog(jobID);
        this.scheduleJobPoll();
        if (notify) this.toast(`Задача ${jobID} открыта.`, "success");
      } catch (error) {
        if (notify) this.toast(error.message, "error");
      }
    }

    scheduleJobPoll() {
      if (this.jobPollTimer) window.clearTimeout(this.jobPollTimer);
      if (!this.currentJob) return;
      const delay = TERMINAL_JOB_STATUSES.has(this.currentJob.status) ? 8000 : POLL_MS;
      this.jobPollTimer = window.setTimeout(async () => {
        if (!this.currentJob) return;
        await this.openJob(this.currentJob.id, false);
      }, delay);
    }

    renderJob(job) {
      $("job-empty")?.setAttribute("hidden", "");
      $("job-dashboard")?.removeAttribute("hidden");
      if ($("job-status-badge")) $("job-status-badge").textContent = STATUS_LABELS[job.status] || job.status;
      if ($("job-id-label")) $("job-id-label").textContent = job.id;
      const done = Number(job.fragments_ready || 0) + Number(job.fragments_warnings || 0) + Number(job.fragments_failed || 0);
      const total = Math.max(1, Number(job.fragments_count || 0));
      const percent = Math.min(100, Math.round((done / total) * 100));
      if ($("job-progress-bar")) {
        $("job-progress-bar").value = percent;
        $("job-progress-bar").textContent = `${percent}%`;
      }
      if ($("job-progress-label")) $("job-progress-label").textContent = `${percent}%`;
      if ($("job-updated-label")) $("job-updated-label").textContent = `Обновлено ${new Date(job.updated_at).toLocaleTimeString()}`;
      const counters = {
        "counter-total": job.fragments_count,
        "counter-pending": job.fragments_pending,
        "counter-ready": job.fragments_ready,
        "counter-warnings": job.fragments_warnings,
        "counter-failed": job.fragments_failed,
      };
      for (const [id, value] of Object.entries(counters)) if ($(id)) $(id).textContent = String(value || 0);
      if ($("job-settings-snapshot")) $("job-settings-snapshot").textContent = JSON.stringify(job.generation_settings, null, 2);
      const download = $("download-link");
      if (download) {
        if (job.status === "completed") {
          download.href = `/v1/job/${encodeURIComponent(job.id)}/audio.zip`;
          download.classList.remove("is-disabled");
          download.removeAttribute("aria-disabled");
          download.removeAttribute("tabindex");
        } else {
          download.removeAttribute("href");
          download.classList.add("is-disabled");
          download.setAttribute("aria-disabled", "true");
          download.tabIndex = -1;
        }
      }
    }

    async loadFragmentCatalog(jobID) {
      try {
        const catalog = await this.api.request(`/v1/job/${encodeURIComponent(jobID)}/chapters`);
        this.currentCatalog = catalog;
        this.renderChapters(catalog.chapters || []);
        this.renderFragments(catalog.fragments || []);
      } catch (error) {
        if (error.status !== 409) this.toast(error.message, "error");
      }
    }

    renderChapters(chapters) {
      const section = $("chapter-downloads");
      const list = $("chapter-downloads-list");
      const status = $("chapter-downloads-status");
      if (!section || !list) return;
      section.removeAttribute("hidden");
      clear(list);
      if (!chapters.length) {
        if (status) status.textContent = "Главы появятся после создания фрагментов.";
        return;
      }
      const readyCount = chapters.filter((chapter) => chapter.ready).length;
      if (status) status.textContent = `Готово к скачиванию: ${readyCount} из ${chapters.length}. FLAC собирается только по клику.`;
      for (const chapter of chapters) {
        const card = document.createElement("article");
        card.className = "chapter-download-card";
        card.appendChild(text("strong", `${String(chapter.chapter_number).padStart(2, "0")} · ${chapter.title || "Без названия"}`));
        card.appendChild(text(
          "span",
          `${chapter.fragments_ready}/${chapter.fragments_count} фрагм. · ${formatDuration(chapter.duration_ms)}`,
        ));
        if (chapter.ready && chapter.audio_url) {
          const link = document.createElement("a");
          link.className = "button button--download";
          link.href = chapter.audio_url;
          link.download = chapter.audio_filename || `chapter-${chapter.chapter_number}.flac`;
          link.textContent = "Скачать FLAC";
          card.appendChild(link);
        } else {
          card.appendChild(text("small", "Скачивание откроется, когда вся глава будет готова."));
        }
        list.appendChild(card);
      }
    }

    renderFragments(fragments) {
      const section = $("warnings-section");
      const list = $("warnings-list");
      if (!section || !list) return;
      section.removeAttribute("hidden");
      clear(list);
      this.selectedFragments.clear();
      const visible = fragments.filter((fragment) => fragment.status !== "pending" || fragment.attempt > 0);
      if ($("warnings-summary")) {
        $("warnings-summary").textContent = visible.length
          ? `Показано ${visible.length} из ${fragments.length} фрагментов.`
          : "Первый фрагмент ещё готовится.";
      }
      for (const fragment of visible) list.appendChild(this.fragmentCard(fragment));
      this.updateRetryButtons();
    }

    fragmentCard(fragment) {
      const card = document.createElement("article");
      card.className = `warning-card warning-card--${fragment.status}`;
      card.dataset.fragmentId = fragment.id;

      const heading = document.createElement("div");
      heading.className = "warning-card-heading";
      const identity = text(
        "h3",
        `Глава ${fragment.chapter_number} · фрагмент ${fragment.ordinal}`,
      );
      heading.appendChild(identity);
      heading.appendChild(text("span", STATUS_LABELS[fragment.status] || fragment.status, "status-badge"));
      card.appendChild(heading);
      if (fragment.chapter_title) card.appendChild(text("p", fragment.chapter_title, "field-hint"));

      if (RETRYABLE_STATUSES.has(fragment.status)) {
        const selection = document.createElement("label");
        selection.className = "warning-selection";
        const checkbox = document.createElement("input");
        checkbox.type = "checkbox";
        checkbox.addEventListener("change", () => {
          if (checkbox.checked) this.selectedFragments.add(fragment.id);
          else this.selectedFragments.delete(fragment.id);
          this.updateRetryButtons();
        });
        selection.appendChild(checkbox);
        selection.appendChild(document.createTextNode(" выбрать для повтора"));
        card.appendChild(selection);
      }

      if (fragment.audio_available) {
        const audio = document.createElement("audio");
        audio.controls = true;
        audio.preload = "none";
        audio.src = `/v1/fragment/${encodeURIComponent(fragment.id)}/audio.wav`;
        card.appendChild(audio);
        card.appendChild(text("small", `Длительность ${formatDuration(fragment.duration_ms)}`));
      }

      const label = text("label", "Текст фрагмента");
      const editor = document.createElement("textarea");
      editor.rows = 4;
      editor.maxLength = 20000;
      editor.value = fragment.text || "";
      editor.disabled = !EDITABLE_STATUSES.has(fragment.status);
      label.appendChild(editor);
      card.appendChild(label);

      if (fragment.stt_text) {
        const transcript = document.createElement("details");
        const summary = text("summary", "Распознанный текст");
        transcript.appendChild(summary);
        transcript.appendChild(text("p", fragment.stt_text));
        card.appendChild(transcript);
      }
      if (fragment.warning_code) {
        card.appendChild(text("p", `Причина: ${fragment.warning_code}`, "warning-reason"));
      }
      if (fragment.error) card.appendChild(text("p", fragment.error, "warning-reason"));

      const actions = document.createElement("div");
      actions.className = "warning-actions";
      if (EDITABLE_STATUSES.has(fragment.status)) {
        actions.appendChild(button("Сохранить и переозвучить", "button button--primary", async () => {
          const newText = editor.value.trim();
          if (!newText || newText === fragment.text) {
            this.toast("Измените текст перед сохранением.", "error");
            return;
          }
          await this.saveFragment(fragment.id, newText);
        }));
      }
      if (fragment.status === "warning" && fragment.audio_available) {
        actions.appendChild(button("Принять аудио", "button button--quiet", () => this.approveFragment(fragment.id)));
      }
      card.appendChild(actions);
      return card;
    }

    async saveFragment(fragmentID, newText) {
      const operation = `edit:${fragmentID}`;
      if (this.busyOperations.has(operation)) return;
      this.busyOperations.add(operation);
      try {
        await this.api.request(`/v1/fragment/${encodeURIComponent(fragmentID)}`, {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ new_text: newText }),
        });
        this.toast("Текст сохранён, фрагмент поставлен на переозвучивание.", "success");
        if (this.currentJob) await this.openJob(this.currentJob.id, false);
      } catch (error) {
        this.toast(error.message, "error");
      } finally {
        this.busyOperations.delete(operation);
      }
    }

    async approveFragment(fragmentID) {
      try {
        await this.api.request(`/v1/fragment/${encodeURIComponent(fragmentID)}/approve`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ reason: "Проверено в Web UI" }),
        });
        this.toast("Аудио принято вручную.", "success");
        if (this.currentJob) await this.openJob(this.currentJob.id, false);
      } catch (error) {
        this.toast(error.message, "error");
      }
    }

    async retrySelected() {
      if (!this.currentJob || !this.selectedFragments.size) return;
      await this.retryFragments(Array.from(this.selectedFragments));
    }

    async retryAll() {
      if (!this.currentJob) return;
      await this.retryFragments([]);
    }

    async retryFragments(fragmentIDs) {
      try {
        await this.api.request(`/v1/job/${encodeURIComponent(this.currentJob.id)}/retry/warnings`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ fragment_ids: fragmentIDs }),
        });
        this.toast("Повторная генерация поставлена в очередь.", "success");
        await this.openJob(this.currentJob.id, false);
      } catch (error) {
        this.toast(error.message, "error");
      }
    }

    updateRetryButtons() {
      if ($("retry-selected-button")) $("retry-selected-button").disabled = this.selectedFragments.size === 0;
      if ($("retry-all-button")) {
        const hasRetryable = Boolean(this.currentCatalog?.fragments?.some((fragment) => RETRYABLE_STATUSES.has(fragment.status)));
        $("retry-all-button").disabled = !hasRetryable;
      }
    }

    async refreshJobs(notify = false) {
      if (this.busyOperations.has("jobs-refresh")) return;
      this.busyOperations.add("jobs-refresh");
      try {
        const status = $("jobs-status-filter")?.value || "active";
        const parameters = new URLSearchParams({
          status,
          limit: String(JOBS_PAGE_SIZE),
          offset: String(this.jobsPage * JOBS_PAGE_SIZE),
        });
        const payload = await this.api.request(`/v1/jobs?${parameters.toString()}`);
        this.jobsTotal = Number(payload.total || 0);
        this.renderJobs(payload.jobs || []);
        if ($("jobs-monitor-summary")) $("jobs-monitor-summary").textContent = `Найдено задач: ${this.jobsTotal}`;
        if ($("jobs-refreshed-at")) $("jobs-refreshed-at").textContent = `Обновлено ${new Date().toLocaleTimeString()}`;
        this.renderJobsPagination();
        if (notify) this.toast("Список задач обновлён.", "success");
      } catch (error) {
        if (notify) this.toast(error.message, "error");
      } finally {
        this.busyOperations.delete("jobs-refresh");
        $("jobs-list")?.setAttribute("aria-busy", "false");
      }
    }

    renderJobs(jobs) {
      const list = $("jobs-list");
      if (!list) return;
      clear(list);
      if (!jobs.length) {
        list.appendChild(text("p", "Задач с выбранным статусом нет.", "empty-state"));
        return;
      }
      for (const job of jobs) {
        const card = document.createElement("article");
        card.className = "jobs-card";
        card.setAttribute("role", "listitem");
        const title = text("strong", STATUS_LABELS[job.status] || job.status);
        card.appendChild(title);
        card.appendChild(text("code", job.id));
        card.appendChild(text(
          "span",
          `${job.fragments_ready || 0}/${job.fragments_count || 0} готово · ${job.fragments_warnings || 0} warning`,
        ));
        card.appendChild(button("Открыть", "button button--quiet", () => this.openJob(job.id, true)));
        list.appendChild(card);
      }
    }

    renderJobsPagination() {
      const pages = Math.max(1, Math.ceil(this.jobsTotal / JOBS_PAGE_SIZE));
      if (this.jobsPage >= pages) this.jobsPage = pages - 1;
      if ($("jobs-page-label")) $("jobs-page-label").textContent = `Страница ${this.jobsPage + 1} из ${pages}`;
      if ($("jobs-prev-button")) $("jobs-prev-button").disabled = this.jobsPage === 0;
      if ($("jobs-next-button")) $("jobs-next-button").disabled = (this.jobsPage + 1) * JOBS_PAGE_SIZE >= this.jobsTotal;
    }

    async copyJobID() {
      if (!this.currentJob) return;
      try {
        await navigator.clipboard.writeText(this.currentJob.id);
        this.toast("ID задачи скопирован.", "success");
      } catch (_) {
        this.toast("Не удалось скопировать ID.", "error");
      }
    }

    updateReferenceCount() {
      if ($("reference-count")) $("reference-count").textContent = String($("reference-text")?.value.length || 0);
    }

    setButtonBusy(id, busy, label) {
      const node = $(id);
      if (!node) return;
      node.disabled = busy;
      const labelNode = node.querySelector("span:last-child") || node;
      labelNode.textContent = label;
    }

    toast(message, kind = "info") {
      const region = $("toast-region");
      if (!region) return;
      const node = text("div", message, `toast toast--${kind}`);
      region.appendChild(node);
      window.setTimeout(() => node.remove(), 5000);
    }
  }

  window.addEventListener("DOMContentLoaded", () => {
    const app = new AudiobookApp();
    app.init();
  });
})();
