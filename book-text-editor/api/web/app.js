(() => {
  "use strict";

  const STORAGE = Object.freeze({
    books: "audiobook-ui.books.v1",
    lastJobID: "audiobook-ui.last-job-id.v1",
    generationSettings: "audiobook-ui.generation-settings.v1",
    activeRewrite: "audiobook-ui.active-rewrite.v1",
  });

  const DEFAULT_GENERATION_SETTINGS = Object.freeze({
    omnivoice: Object.freeze({
      num_steps: 32,
      guidance_scale: 2,
      speed: 1,
      normalize_text: true,
      denoise: true,
      t_shift: 0.1,
      layer_penalty_factor: 5,
      position_temperature: 5,
      class_temperature: 0,
      preprocess_prompt: true,
      postprocess_output: true,
      audio_chunk_duration: 15,
      audio_chunk_threshold: 30,
      pad_duration: 0.1,
      fade_duration: 0.1,
    }),
    whisper: Object.freeze({
      beam_size: 5,
      patience: 1,
      temperature: 0,
      vad_filter: false,
      word_timestamps: true,
    }),
    automatic_warning_retries: 1,
  });

  const TERMINAL_JOB_STATUSES = new Set([
    "completed",
    "completed_with_warnings",
    "failed",
  ]);

  const JOBS_PAGE_SIZE = 8;
  const JOBS_POLL_INTERVAL_MS = 4000;
  const REWRITE_POLL_INTERVAL_MS = 1800;
  const REWRITE_TERMINAL_STATUSES = new Set([
    "completed",
    "completed_with_errors",
    "failed",
  ]);

  const JOB_STATUS_LABELS = Object.freeze({
    queued: "В очереди",
    running: "Генерация идёт",
    completed: "Готово",
    completed_with_warnings: "Нужна проверка",
    failed: "Завершено с ошибками",
  });

  const WARNING_LABELS = Object.freeze({
    transcript_mismatch: "Распознанный текст сильно отличается",
    audio_warning: "Аудио требует проверки",
    text_edited: "Текст отредактирован",
    text_rewritten: "Текст переписан локальной моделью",
    text_restored: "Восстановлена предыдущая версия текста",
  });

  const TEXT_ONLY_WARNING_CODES = new Set([
    "text_edited",
    "text_rewritten",
    "text_restored",
  ]);

  const REVISION_SOURCE_LABELS = Object.freeze({
    original: "Исходная версия",
    manual: "Ручная правка",
    ai: "AI-правка",
    restore: "Восстановленная версия",
  });

  const REWRITE_STATUS_LABELS = Object.freeze({
    queued: "Задача ожидает запуска",
    running: "Локальная модель переписывает фрагменты",
    completed: "AI-правка завершена",
    completed_with_errors: "AI-правка завершена с ошибками",
    failed: "AI-правка не выполнена",
  });

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
      if (!headers.has("Accept")) {
        headers.set("Accept", "application/json");
      }

      let response;
      try {
        response = await fetch(path, {
          method: options.method || "GET",
          body: options.body,
          headers,
          credentials: "same-origin",
        });
      } catch (error) {
        throw new APIError(
          0,
          "NETWORK_ERROR",
          "Go API недоступен. Проверьте, что сервис запущен.",
        );
      }

      if (!response.ok) {
        let problem = null;
        try {
          problem = await response.json();
        } catch (_) {
          // The API normally returns JSON problems, but a proxy may not.
        }
        throw new APIError(
          response.status,
          problem && problem.code,
          problem && problem.error
            ? problem.error
            : `Запрос завершился с кодом ${response.status}`,
        );
      }

      if (response.status === 204) {
        return null;
      }
      const contentType = response.headers.get("Content-Type") || "";
      if (contentType.includes("application/json")) {
        return response.json();
      }
      return response;
    }

    health() {
      return this.request("/healthz");
    }

    uploadBook(file) {
      const body = new FormData();
      body.set("file", file);
      return this.request("/v1/book", { method: "POST", body });
    }

    getBook(bookID) {
      return this.request(`/v1/books/${encodeURIComponent(bookID)}`);
    }

    uploadVoice({ file, name, referenceText }) {
      const body = new FormData();
      body.set("reference_audio", file);
      body.set("reference_text", referenceText);
      if (name) {
        body.set("name", name);
      }
      return this.request("/v1/voice", { method: "POST", body });
    }

    listVoices() {
      return this.request("/v1/voices");
    }

    generate(bookID, voiceID, settings) {
      return this.request(
        `/v1/generate/book/${encodeURIComponent(bookID)}` +
          `/voice/${encodeURIComponent(voiceID)}`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(settings),
        },
      );
    }

    getJob(jobID) {
      return this.request(`/v1/job/${encodeURIComponent(jobID)}`);
    }

    listJobs({ status, limit, offset }) {
      const parameters = new URLSearchParams({
        status,
        limit: String(limit),
        offset: String(offset),
      });
      return this.request(`/v1/jobs?${parameters.toString()}`);
    }

    getWarnings(jobID) {
      return this.request(
        `/v1/job/${encodeURIComponent(jobID)}/warnings`,
      );
    }

    editFragment(fragmentID, newText) {
      return this.request(
        `/v1/fragment/${encodeURIComponent(fragmentID)}`,
        {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ new_text: newText }),
        },
      );
    }

    retryWarnings(jobID, fragmentIDs) {
      return this.request(
        `/v1/job/${encodeURIComponent(jobID)}/retry/warnings`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ fragment_ids: fragmentIDs }),
        },
      );
    }

    approveFragment(fragmentID, reason) {
      return this.request(
        `/v1/fragment/${encodeURIComponent(fragmentID)}/approve`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ reason }),
        },
      );
    }

    getFragmentRevisions(fragmentID) {
      return this.request(
        `/v1/fragment/${encodeURIComponent(fragmentID)}/revisions`,
      );
    }

    restoreFragmentRevision(fragmentID, revisionID, reason) {
      return this.request(
        `/v1/fragment/${encodeURIComponent(fragmentID)}/revisions/` +
          `${encodeURIComponent(revisionID)}/restore`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ reason }),
        },
      );
    }

    getRewriteModels() {
      return this.request("/v1/rewrite/models");
    }

    rewriteWarnings(jobID, payload) {
      return this.request(
        `/v1/job/${encodeURIComponent(jobID)}/rewrite/warnings`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload),
        },
      );
    }

    getRewriteTask(rewriteID) {
      return this.request(`/v1/rewrite/${encodeURIComponent(rewriteID)}`);
    }

    listChapters(jobID, includeFragments = false) {
      const suffix = includeFragments ? "" : "?include_fragments=0";
      return this.request(
        `/v1/job/${encodeURIComponent(jobID)}/chapters${suffix}`,
      );
    }
  }

  class AudiobookApp {
    constructor(documentRoot) {
      this.document = documentRoot;
      this.api = new APIClient();
      this.books = this.loadBooks();
      this.voices = [];
      this.currentJob = null;
      this.currentJobID = this.readStorage(STORAGE.lastJobID) || "";
      this.selectedWarningIDs = new Set();
      this.warningFragments = [];
      this.expandedRevisionIDs = new Set();
      this.rewriteModelsResponse = null;
      this.rewritePromptCustomized = false;
      this.rewritePollToken = 0;
      this.currentRewrite = null;
      this.busyOperations = new Set();
      this.pollToken = 0;
      this.chapterRequestToken = 0;
      this.chapterDataJobID = "";
      this.chapterLoadingJobID = "";
      this.jobsFilter = "active";
      this.jobsLimit = JOBS_PAGE_SIZE;
      this.jobsOffset = 0;
      this.jobsTotal = 0;
      this.jobsPollToken = 0;
      this.jobsRefreshInFlight = false;

      this.nodes = {
        healthIndicator: this.byID("health-indicator"),
        healthLabel: this.byID("health-label"),

        bookForm: this.byID("book-upload-form"),
        bookFile: this.byID("book-file"),
        bookFileLabel: this.byID("book-file-label"),
        bookUploadButton: this.byID("book-upload-button"),
        bookSelect: this.byID("book-select"),
        bookSummary: this.byID("book-summary"),

        voiceForm: this.byID("voice-upload-form"),
        voiceName: this.byID("voice-name"),
        voiceFile: this.byID("voice-file"),
        voiceFileLabel: this.byID("voice-file-label"),
        referenceText: this.byID("reference-text"),
        referenceCount: this.byID("reference-count"),
        voiceUploadButton: this.byID("voice-upload-button"),
        refreshVoicesButton: this.byID("refresh-voices-button"),
        voiceSelect: this.byID("voice-select"),
        voiceList: this.byID("voice-list"),

        selectedBookLabel: this.byID("selected-book-label"),
        selectedVoiceLabel: this.byID("selected-voice-label"),
        generateButton: this.byID("generate-button"),
        generationSettings: this.byID("generation-settings"),
        generationSettingsReset: this.byID("generation-settings-reset"),
        ttsNumSteps: this.byID("tts-num-steps"),
        ttsGuidanceScale: this.byID("tts-guidance-scale"),
        ttsSpeed: this.byID("tts-speed"),
        ttsNormalizeText: this.byID("tts-normalize-text"),
        ttsDenoise: this.byID("tts-denoise"),
        ttsTShift: this.byID("tts-t-shift"),
        ttsLayerPenalty: this.byID("tts-layer-penalty"),
        ttsPositionTemperature: this.byID("tts-position-temperature"),
        ttsClassTemperature: this.byID("tts-class-temperature"),
        ttsPreprocessPrompt: this.byID("tts-preprocess-prompt"),
        ttsPostprocessOutput: this.byID("tts-postprocess-output"),
        ttsChunkDuration: this.byID("tts-chunk-duration"),
        ttsChunkThreshold: this.byID("tts-chunk-threshold"),
        ttsPadDuration: this.byID("tts-pad-duration"),
        ttsFadeDuration: this.byID("tts-fade-duration"),
        sttBeamSize: this.byID("stt-beam-size"),
        sttPatience: this.byID("stt-patience"),
        sttTemperature: this.byID("stt-temperature"),
        sttVADFilter: this.byID("stt-vad-filter"),
        sttWordTimestamps: this.byID("stt-word-timestamps"),
        warningRetries: this.byID("warning-retries"),

        jobsStatusFilter: this.byID("jobs-status-filter"),
        jobsRefreshButton: this.byID("jobs-refresh-button"),
        jobsSummary: this.byID("jobs-monitor-summary"),
        jobsRefreshedAt: this.byID("jobs-refreshed-at"),
        jobsList: this.byID("jobs-list"),
        jobsPrevButton: this.byID("jobs-prev-button"),
        jobsNextButton: this.byID("jobs-next-button"),
        jobsPageLabel: this.byID("jobs-page-label"),

        jobResumeForm: this.byID("job-resume-form"),
        jobIDInput: this.byID("job-id-input"),
        jobEmpty: this.byID("job-empty"),
        jobDashboard: this.byID("job-dashboard"),
        jobStatusBadge: this.byID("job-status-badge"),
        jobIDLabel: this.byID("job-id-label"),
        copyJobIDButton: this.byID("copy-job-id-button"),
        downloadLink: this.byID("download-link"),
        progressBar: this.byID("job-progress-bar"),
        progressLabel: this.byID("job-progress-label"),
        jobUpdatedLabel: this.byID("job-updated-label"),
        counterTotal: this.byID("counter-total"),
        counterPending: this.byID("counter-pending"),
        counterReady: this.byID("counter-ready"),
        counterWarnings: this.byID("counter-warnings"),
        counterFailed: this.byID("counter-failed"),
        jobSettingsSnapshot: this.byID("job-settings-snapshot"),
        chapterDownloads: this.byID("chapter-downloads"),
        chapterDownloadsStatus: this.byID("chapter-downloads-status"),
        chapterDownloadsList: this.byID("chapter-downloads-list"),

        warningsSection: this.byID("warnings-section"),
        warningsSummary: this.byID("warnings-summary"),
        warningsList: this.byID("warnings-list"),
        retrySelectedButton: this.byID("retry-selected-button"),
        retryAllButton: this.byID("retry-all-button"),
        rewriteModelSelect: this.byID("rewrite-model-select"),
        rewriteModelRefreshButton: this.byID(
          "rewrite-model-refresh-button",
        ),
        rewritePrompt: this.byID("rewrite-prompt"),
        rewritePromptResetButton: this.byID(
          "rewrite-prompt-reset-button",
        ),
        rewriteTemperature: this.byID("rewrite-temperature"),
        rewriteTemperatureRange: this.byID("rewrite-temperature-range"),
        rewriteTopK: this.byID("rewrite-top-k"),
        rewriteTopKRange: this.byID("rewrite-top-k-range"),
        rewriteTopP: this.byID("rewrite-top-p"),
        rewriteTopPRange: this.byID("rewrite-top-p-range"),
        rewriteMinP: this.byID("rewrite-min-p"),
        rewriteMinPRange: this.byID("rewrite-min-p-range"),
        rewriteRepeatPenalty: this.byID("rewrite-repeat-penalty"),
        rewriteRepeatPenaltyRange: this.byID(
          "rewrite-repeat-penalty-range",
        ),
        rewriteMaxTokens: this.byID("rewrite-max-tokens"),
        rewriteMaxTokensRange: this.byID("rewrite-max-tokens-range"),
        rewriteModelStatus: this.byID("rewrite-model-status"),
        rewriteSelectedButton: this.byID("rewrite-selected-button"),
        rewriteAllButton: this.byID("rewrite-all-button"),
        rewriteProgressRegion: this.byID("rewrite-progress-region"),
        rewriteProgressLabel: this.byID("rewrite-progress-label"),
        rewriteProgressPercent: this.byID("rewrite-progress-percent"),
        rewriteProgressBar: this.byID("rewrite-progress-bar"),
        rewriteProgressDetails: this.byID("rewrite-progress-details"),
        rewriteFailures: this.byID("rewrite-failures"),
        toastRegion: this.byID("toast-region"),
      };
    }

    init() {
      this.restoreGenerationSettings();
      this.bindEvents();
      this.bindFilePicker(
        this.nodes.bookFile,
        this.nodes.bookFileLabel,
        "До 50 МБ, без ZIP-сжатия",
      );
      this.bindFilePicker(
        this.nodes.voiceFile,
        this.nodes.voiceFileLabel,
        "До 5 МБ",
      );
      this.renderBooks();
      this.renderSelectionSummary();
      this.checkHealth();
      this.refreshVoices();
      this.startJobsMonitor();

      if (this.currentJobID) {
        this.nodes.jobIDInput.value = this.currentJobID;
        this.openJob(this.currentJobID, false);
      }
      this.resumeStoredRewrite();
    }

    bindEvents() {
      this.nodes.bookForm.addEventListener("submit", (event) => {
        event.preventDefault();
        this.uploadBook();
      });
      this.nodes.voiceForm.addEventListener("submit", (event) => {
        event.preventDefault();
        this.uploadVoice();
      });
      this.nodes.bookSelect.addEventListener("change", () => {
        this.renderBookSummary();
        this.renderSelectionSummary();
        this.refreshSelectedBook();
      });
      this.nodes.voiceSelect.addEventListener("change", () => {
        this.renderSelectionSummary();
      });
      this.nodes.referenceText.addEventListener("input", () => {
        this.nodes.referenceCount.textContent = String(
          this.nodes.referenceText.value.length,
        );
      });
      this.nodes.refreshVoicesButton.addEventListener("click", () => {
        this.refreshVoices();
      });
      this.nodes.generateButton.addEventListener("click", () => {
        this.startGeneration();
      });
      for (const field of this.generationSettingFields()) {
        field.input.addEventListener("change", () => {
          const settings = this.collectGenerationSettings(false);
          if (settings) {
            this.persistGenerationSettings(settings);
          }
        });
      }
      this.nodes.generationSettingsReset.addEventListener("click", () => {
        this.applyGenerationSettings(DEFAULT_GENERATION_SETTINGS);
        this.persistGenerationSettings(DEFAULT_GENERATION_SETTINGS);
        this.notify("Рекомендуемые настройки генерации восстановлены.");
      });
      this.nodes.jobsStatusFilter.addEventListener("change", () => {
        this.jobsFilter = this.nodes.jobsStatusFilter.value;
        this.jobsOffset = 0;
        this.nodes.jobsSummary.textContent = "Обновляем список задач…";
        this.startJobsMonitor();
      });
      this.nodes.jobsRefreshButton.addEventListener("click", () => {
        this.startJobsMonitor(true);
      });
      this.nodes.jobsPrevButton.addEventListener("click", () => {
        if (this.jobsOffset === 0) {
          return;
        }
        this.jobsOffset = Math.max(0, this.jobsOffset - this.jobsLimit);
        this.startJobsMonitor();
      });
      this.nodes.jobsNextButton.addEventListener("click", () => {
        if (this.jobsOffset + this.jobsLimit >= this.jobsTotal) {
          return;
        }
        this.jobsOffset += this.jobsLimit;
        this.startJobsMonitor();
      });
      this.nodes.jobResumeForm.addEventListener("submit", (event) => {
        event.preventDefault();
        const jobID = this.nodes.jobIDInput.value.trim();
        if (!jobID) {
          this.notify("Введите ID задачи.", "error");
          return;
        }
        this.openJob(jobID, true);
      });
      this.nodes.copyJobIDButton.addEventListener("click", () => {
        this.copyCurrentJobID();
      });
      this.nodes.downloadLink.addEventListener("click", (event) => {
        if (this.nodes.downloadLink.getAttribute("aria-disabled") === "true") {
          event.preventDefault();
          this.notify(
            "Архив с готовыми главами FLAC станет доступен после генерации.",
            "error",
          );
        }
      });
      this.nodes.retrySelectedButton.addEventListener("click", () => {
        this.retryWarnings(Array.from(this.selectedWarningIDs));
      });
      this.nodes.retryAllButton.addEventListener("click", () => {
        this.retryWarnings([]);
      });
      this.nodes.rewriteModelRefreshButton.addEventListener("click", () => {
        this.loadRewriteModels(true);
      });
      this.nodes.rewriteModelSelect.addEventListener("change", () => {
        this.renderSelectedRewriteModelStatus();
      });
      this.nodes.rewritePrompt.addEventListener("input", () => {
        this.rewritePromptCustomized = true;
      });
      this.nodes.rewritePromptResetButton.addEventListener("click", () => {
        const defaults = this.rewriteModelsResponse;
        if (!defaults) {
          return;
        }
        this.nodes.rewritePrompt.value = defaults.default_prompt || "";
        this.rewritePromptCustomized = false;
        this.notify("Инструкция модели восстановлена.");
      });
      this.nodes.rewriteSelectedButton.addEventListener("click", () => {
        this.startRewrite(Array.from(this.selectedWarningIDs));
      });
      this.nodes.rewriteAllButton.addEventListener("click", () => {
        this.startRewrite([]);
      });
    }

    bindFilePicker(input, label, fallback) {
      const picker = input.closest(".file-picker");
      const showSelection = () => {
        const file = input.files && input.files[0];
        label.textContent = file ? this.describeFile(file) : fallback;
      };
      input.addEventListener("change", showSelection);

      if (!picker) {
        return;
      }
      for (const eventName of ["dragenter", "dragover"]) {
        picker.addEventListener(eventName, (event) => {
          event.preventDefault();
          picker.classList.add("is-dragging");
        });
      }
      for (const eventName of ["dragleave", "drop"]) {
        picker.addEventListener(eventName, (event) => {
          event.preventDefault();
          picker.classList.remove("is-dragging");
        });
      }
      picker.addEventListener("drop", (event) => {
        const files = event.dataTransfer && event.dataTransfer.files;
        if (!files || files.length === 0) {
          return;
        }
        try {
          input.files = files;
          showSelection();
        } catch (_) {
          this.notify("Перетащите файл ещё раз или выберите его вручную.", "error");
        }
      });
    }

    async checkHealth() {
      try {
        const health = await this.api.health();
        if (!health || health.status !== "ok") {
          throw new Error("unexpected health response");
        }
        this.nodes.healthIndicator.className =
          "health-indicator health-indicator--online";
        this.nodes.healthLabel.textContent = "API доступен";
      } catch (_) {
        this.nodes.healthIndicator.className =
          "health-indicator health-indicator--offline";
        this.nodes.healthLabel.textContent = "API недоступен";
      }
    }

    async uploadBook() {
      const file = this.nodes.bookFile.files && this.nodes.bookFile.files[0];
      if (!file) {
        this.notify("Выберите FB2- или FB2.ZIP-файл.", "error");
        return;
      }

      await this.withBusy(
        "upload-book",
        this.nodes.bookUploadButton,
        async () => {
          const book = await this.api.uploadBook(file);
          this.upsertBook(book);
          this.renderBooks(book.id);
          this.nodes.bookForm.reset();
          this.nodes.bookFileLabel.textContent = "До 50 МБ · .fb2, .zip или .fb2.zip";
          this.notify(
            `Книга «${book.title || "Без названия"}» готова к генерации.`,
            "success",
          );
        },
      );
    }

    async refreshSelectedBook() {
      const bookID = this.nodes.bookSelect.value;
      if (!bookID || this.busyOperations.has(`book-${bookID}`)) {
        return;
      }
      this.busyOperations.add(`book-${bookID}`);
      try {
        const book = await this.api.getBook(bookID);
        this.upsertBook(book);
        this.renderBooks(bookID);
      } catch (error) {
        this.notifyError(error, "Не удалось обновить сведения о книге");
      } finally {
        this.busyOperations.delete(`book-${bookID}`);
      }
    }

    async uploadVoice() {
      const file = this.nodes.voiceFile.files && this.nodes.voiceFile.files[0];
      const referenceText = this.nodes.referenceText.value.trim();
      if (!file) {
        this.notify("Выберите FLAC- или WAV-файл.", "error");
        return;
      }
      if (!referenceText) {
        this.notify("Введите точную расшифровку голосового референса.", "error");
        return;
      }

      await this.withBusy(
        "upload-voice",
        this.nodes.voiceUploadButton,
        async () => {
          const voice = await this.api.uploadVoice({
            file,
            name: this.nodes.voiceName.value.trim(),
            referenceText,
          });
          this.nodes.voiceForm.reset();
          this.nodes.referenceCount.textContent = "0";
          this.nodes.voiceFileLabel.textContent = "До 5 МБ";
          await this.refreshVoices(voice.id);
          this.notify(
            `Голос «${voice.name || "Без названия"}» создан.`,
            "success",
          );
        },
      );
    }

    async refreshVoices(preferredVoiceID = "") {
      await this.withBusy(
        "refresh-voices",
        this.nodes.refreshVoicesButton,
        async () => {
          const response = await this.api.listVoices();
          this.voices = Array.isArray(response.voices) ? response.voices : [];
          this.renderVoices(preferredVoiceID);
        },
      );
    }

    generationSettingFields() {
      return [
        {
          input: this.nodes.ttsNumSteps,
          section: "omnivoice",
          key: "num_steps",
          label: "Шаги диффузии",
          integer: true,
        },
        {
          input: this.nodes.ttsGuidanceScale,
          section: "omnivoice",
          key: "guidance_scale",
          label: "Guidance scale",
        },
        {
          input: this.nodes.ttsSpeed,
          section: "omnivoice",
          key: "speed",
          label: "Скорость речи",
        },
        {
          input: this.nodes.ttsNormalizeText,
          section: "omnivoice",
          key: "normalize_text",
          label: "Нормализация текста",
          boolean: true,
        },
        {
          input: this.nodes.ttsDenoise,
          section: "omnivoice",
          key: "denoise",
          label: "Denoise-токен",
          boolean: true,
        },
        {
          input: this.nodes.ttsTShift,
          section: "omnivoice",
          key: "t_shift",
          label: "T-shift",
        },
        {
          input: this.nodes.ttsLayerPenalty,
          section: "omnivoice",
          key: "layer_penalty_factor",
          label: "Layer penalty",
        },
        {
          input: this.nodes.ttsPositionTemperature,
          section: "omnivoice",
          key: "position_temperature",
          label: "Position temperature",
        },
        {
          input: this.nodes.ttsClassTemperature,
          section: "omnivoice",
          key: "class_temperature",
          label: "Class temperature",
        },
        {
          input: this.nodes.ttsPreprocessPrompt,
          section: "omnivoice",
          key: "preprocess_prompt",
          label: "Обработка voice prompt",
          boolean: true,
        },
        {
          input: this.nodes.ttsPostprocessOutput,
          section: "omnivoice",
          key: "postprocess_output",
          label: "Обработка результата",
          boolean: true,
        },
        {
          input: this.nodes.ttsChunkDuration,
          section: "omnivoice",
          key: "audio_chunk_duration",
          label: "Длина chunk",
        },
        {
          input: this.nodes.ttsChunkThreshold,
          section: "omnivoice",
          key: "audio_chunk_threshold",
          label: "Порог chunking",
        },
        {
          input: this.nodes.ttsPadDuration,
          section: "omnivoice",
          key: "pad_duration",
          label: "Padding",
        },
        {
          input: this.nodes.ttsFadeDuration,
          section: "omnivoice",
          key: "fade_duration",
          label: "Fade",
        },
        {
          input: this.nodes.sttBeamSize,
          section: "whisper",
          key: "beam_size",
          label: "Whisper beam size",
          integer: true,
        },
        {
          input: this.nodes.sttPatience,
          section: "whisper",
          key: "patience",
          label: "Whisper patience",
        },
        {
          input: this.nodes.sttTemperature,
          section: "whisper",
          key: "temperature",
          label: "Whisper temperature",
        },
        {
          input: this.nodes.sttVADFilter,
          section: "whisper",
          key: "vad_filter",
          label: "Whisper VAD filter",
          boolean: true,
        },
        {
          input: this.nodes.sttWordTimestamps,
          section: "whisper",
          key: "word_timestamps",
          label: "Таймкоды слов",
          boolean: true,
        },
        {
          input: this.nodes.warningRetries,
          section: "",
          key: "automatic_warning_retries",
          label: "Количество повторов warning",
          integer: true,
        },
      ];
    }

    restoreGenerationSettings() {
      let stored = null;
      const raw = this.readStorage(STORAGE.generationSettings);
      if (raw) {
        try {
          const parsed = JSON.parse(raw);
          if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
            stored = parsed;
          }
        } catch (_) {
          // Invalid browser state is replaced with the documented defaults.
        }
      }

      this.applyGenerationSettings(
        stored || DEFAULT_GENERATION_SETTINGS,
      );
      const normalized = this.collectGenerationSettings(false);
      if (normalized) {
        this.persistGenerationSettings(normalized);
        return;
      }
      this.applyGenerationSettings(DEFAULT_GENERATION_SETTINGS);
      this.persistGenerationSettings(DEFAULT_GENERATION_SETTINGS);
    }

    applyGenerationSettings(settings) {
      for (const field of this.generationSettingFields()) {
        const defaults = field.section
          ? DEFAULT_GENERATION_SETTINGS[field.section]
          : DEFAULT_GENERATION_SETTINGS;
        const source =
          field.section &&
          settings &&
          typeof settings[field.section] === "object"
            ? settings[field.section]
            : settings;
        const fallback = defaults[field.key];
        let candidate = source && source[field.key];
        if (field.key === "automatic_warning_retries" &&
            typeof candidate === "number") {
          candidate = Math.min(candidate, 1);
        }
        if (field.boolean) {
          field.input.checked =
            typeof candidate === "boolean" ? candidate : fallback;
          continue;
        }
        field.input.value = String(
          typeof candidate === "number" && Number.isFinite(candidate)
            ? candidate
            : fallback,
        );
      }
    }

    collectGenerationSettings(announce = true) {
      const settings = {
        omnivoice: {},
        whisper: {},
        automatic_warning_retries:
          DEFAULT_GENERATION_SETTINGS.automatic_warning_retries,
      };
      for (const field of this.generationSettingFields()) {
        let value;
        if (field.boolean) {
          value = field.input.checked;
        } else {
          value = Number(field.input.value);
          const valid =
            field.input.value !== "" &&
            Number.isFinite(value) &&
            (!field.integer || Number.isInteger(value)) &&
            field.input.checkValidity();
          if (!valid) {
            if (announce) {
              this.nodes.generationSettings.open = true;
              this.notify(
                `${field.label}: укажите значение в допустимом диапазоне.`,
                "error",
              );
              field.input.focus();
            }
            return null;
          }
        }
        if (field.section) {
          settings[field.section][field.key] = value;
        } else {
          settings[field.key] = value;
        }
      }
      return settings;
    }

    persistGenerationSettings(settings) {
      this.writeStorage(
        STORAGE.generationSettings,
        JSON.stringify(settings),
      );
    }

    async startGeneration() {
      const bookID = this.nodes.bookSelect.value;
      const voiceID = this.nodes.voiceSelect.value;
      if (!bookID || !voiceID) {
        this.notify("Выберите книгу и голос.", "error");
        return;
      }
      if (
        this.currentJob &&
        !TERMINAL_JOB_STATUSES.has(this.currentJob.status)
      ) {
        this.notify(
          "Дождитесь завершения текущей задачи или откройте другую.",
          "error",
        );
        return;
      }
      const settings = this.collectGenerationSettings(true);
      if (!settings) {
        return;
      }
      this.persistGenerationSettings(settings);

      await this.withBusy(
        "generate",
        this.nodes.generateButton,
        async () => {
          const response = await this.api.generate(
            bookID,
            voiceID,
            settings,
          );
          const jobID = response.job_id || (response.job && response.job.id);
          if (!jobID) {
            throw new Error("API не вернул ID задачи");
          }
          this.setCurrentJobID(jobID);
          this.currentJob = response.job || null;
          if (this.currentJob) {
            this.renderJob(this.currentJob);
          }
          this.startPolling(jobID);
          this.startJobsMonitor();
          this.byID("job-title").scrollIntoView({
            behavior: "smooth",
            block: "start",
          });
          this.notify("Задача поставлена в очередь.", "success");
        },
      );
    }

    async openJob(jobID, announce) {
      const normalizedJobID = jobID.trim();
      if (!normalizedJobID) {
        return;
      }
      this.setCurrentJobID(normalizedJobID);
      try {
        const job = await this.api.getJob(normalizedJobID);
        this.currentJob = job;
        this.renderJob(job);
        this.ensureBookMetadata(job.book_id);
        if (announce) {
          this.notify("Задача открыта.", "success");
        }
        await this.loadFragmentCatalog(normalizedJobID, true);
        if (TERMINAL_JOB_STATUSES.has(job.status)) {
          return;
        }
        this.startPolling(normalizedJobID);
      } catch (error) {
        this.pollToken += 1;
        this.notifyError(error, "Не удалось открыть задачу");
      }
    }

    startPolling(jobID) {
      this.pollToken += 1;
      const token = this.pollToken;
      let consecutiveErrors = 0;

      const poll = async () => {
        if (token !== this.pollToken) {
          return;
        }
        try {
          const job = await this.api.getJob(jobID);
          if (token !== this.pollToken) {
            return;
          }
          this.currentJob = job;
          this.renderJob(job);
          this.ensureBookMetadata(job.book_id);
          await this.loadFragmentCatalog(jobID);
          consecutiveErrors = 0;

          if (TERMINAL_JOB_STATUSES.has(job.status)) {
            if (job.status === "completed") {
              this.notify("Аудиокнига готова к скачиванию.", "success");
            }
            return;
          }
        } catch (error) {
          if (error instanceof APIError && error.status === 404) {
            this.notifyError(error, "Задача не найдена");
            return;
          }
          consecutiveErrors += 1;
          if (consecutiveErrors === 1) {
            this.notifyError(error, "Не удалось обновить прогресс");
          }
        }

        if (token === this.pollToken) {
          const delay = consecutiveErrors > 0
            ? Math.min(1600 * (2 ** consecutiveErrors), 10000)
            : 5000;
          window.setTimeout(poll, delay);
        }
      };

      poll();
    }

    async loadFragmentCatalog(jobID, announceError = false) {
      try {
        const response = await this.api.listChapters(jobID, false);
        if (jobID !== this.currentJobID) return;
        const chapters = Array.isArray(response.chapters) ? response.chapters : [];
        this.nodes.chapterDownloads.hidden = false;
        this.renderChapterDownloads(
          this.currentJob || { id: jobID, status: "running" },
          chapters,
        );
        this.warningFragments = [];
        this.selectedWarningIDs.clear();
        this.nodes.warningsList.replaceChildren();
        this.nodes.warningsSection.hidden = true;
        this.nodes.warningsSummary.textContent = "";
        this.updateRetrySelectedButton();

        if (response.audio_zip_url) {
          this.nodes.downloadLink.href = response.audio_zip_url;
          this.nodes.downloadLink.download =
            `book-${(this.currentJob && this.currentJob.book_id) || "audio"}-chapters-flac.zip`;
          this.nodes.downloadLink.classList.remove("is-disabled");
          this.nodes.downloadLink.setAttribute("aria-disabled", "false");
          this.nodes.downloadLink.setAttribute("tabindex", "0");
        }
      } catch (error) {
        this.nodes.chapterDownloadsStatus.textContent =
          "Не удалось обновить прогресс глав. Повторим автоматически.";
        if (announceError) this.notifyError(error, "Не удалось загрузить главы задачи");
      }
    }

    async loadWarnings(jobID) {
      await this.loadFragmentCatalog(jobID, true);
    }

    async editWarning(fragment, textarea, button) {
      if (this.isRewriteActive()) {
        this.notify(
          "Дождитесь завершения AI-правки перед ручным редактированием.",
          "error",
        );
        return;
      }
      const newText = textarea.value.trim();
      if (!newText) {
        this.notify("Текст фрагмента не может быть пустым.", "error");
        textarea.focus();
        return;
      }

      await this.withBusy(
        `edit-${fragment.id}`,
        button,
        async () => {
          const updated = await this.api.editFragment(fragment.id, newText);
          textarea.value = updated.text || newText;
          this.selectedWarningIDs.delete(fragment.id);
          if (this.currentJobID) {
            if (this.currentJob) {
              this.currentJob = { ...this.currentJob, status: "queued" };
              this.renderJob(this.currentJob);
            }
            await this.loadFragmentCatalog(this.currentJobID, true);
            this.startPolling(this.currentJobID);
            this.startJobsMonitor();
          }
          this.notify(
            "Правка сохранена и автоматически поставлена на переозвучивание.",
            "success",
          );
        },
      );
    }

    async retryWarnings(fragmentIDs) {
      if (this.isRewriteActive()) {
        this.notify(
          "Дождитесь завершения AI-правки перед повторной генерацией.",
          "error",
        );
        return;
      }
      if (!this.currentJobID) {
        this.notify("Сначала откройте задачу.", "error");
        return;
      }
      if (
        fragmentIDs.length === 0 &&
        this.warningFragments.length === 0
      ) {
        this.notify("Нет фрагментов для повторной генерации.", "error");
        return;
      }

      const button =
        fragmentIDs.length > 0
          ? this.nodes.retrySelectedButton
          : this.nodes.retryAllButton;
      await this.withBusy("retry", button, async () => {
        await this.api.retryWarnings(this.currentJobID, fragmentIDs);
        this.selectedWarningIDs.clear();
        this.warningFragments = [];
        this.nodes.warningsList.replaceChildren();
        this.nodes.warningsSection.hidden = true;
        this.nodes.warningsSummary.textContent = "";
        this.updateRetrySelectedButton();
        if (this.currentJob) {
          this.currentJob = { ...this.currentJob, status: "queued" };
          this.renderJob(this.currentJob);
        }
        this.notify("Фрагменты снова поставлены в очередь.", "success");
        this.startPolling(this.currentJobID);
        this.startJobsMonitor();
      });
    }

    async loadRewriteModels(force = false) {
      if (this.rewriteModelsResponse && !force) {
        this.updateRetrySelectedButton();
        return;
      }
      if (this.busyOperations.has("rewrite-models")) {
        return;
      }

      this.busyOperations.add("rewrite-models");
      this.nodes.rewriteModelRefreshButton.disabled = true;
      this.nodes.rewriteModelRefreshButton.classList.add("is-busy");
      this.nodes.rewriteModelStatus.textContent =
        "Получаем список разрешённых локальных моделей…";
      this.updateRetrySelectedButton();

      try {
        const response = await this.api.getRewriteModels();
        const models = Array.isArray(response && response.models)
          ? response.models
          : [];
        const previousModelID = this.nodes.rewriteModelSelect.value;
        this.rewriteModelsResponse = {
          ...response,
          models,
        };
        this.nodes.rewriteModelSelect.replaceChildren();

        if (models.length === 0) {
          this.nodes.rewriteModelSelect.append(
            new Option("Нет доступных моделей", ""),
          );
          this.nodes.rewriteModelStatus.textContent =
            "Go API не вернул ни одной разрешённой модели.";
          return;
        }

        for (const model of models) {
          const attributes = [];
          if (model.recommended) {
            attributes.push("рекомендуется");
          }
          if (model.loaded) {
            attributes.push("загружена в память");
          } else if (model.available) {
            attributes.push("готова локально");
          } else {
            attributes.push("скачается при запуске");
          }
          const size = this.formatBytes(model.size_bytes);
          if (size) {
            attributes.push(size);
          }
          const name = model.display_name || model.id;
          this.nodes.rewriteModelSelect.append(
            new Option(
              `${name}${attributes.length ? ` · ${attributes.join(" · ")}` : ""}`,
              model.id,
            ),
          );
        }

        const preferredID = models.some(
          (model) => model.id === previousModelID,
        )
          ? previousModelID
          : models.some((model) => model.id === response.default_model_id)
            ? response.default_model_id
            : (models.find((model) => model.recommended) || models[0]).id;
        this.nodes.rewriteModelSelect.value = preferredID;

        if (!this.rewritePromptCustomized) {
          this.nodes.rewritePrompt.value = response.default_prompt || "";
        }
        const maxPromptLength = this.number(
          response.limits && response.limits.max_prompt_chars,
        );
        if (maxPromptLength > 0) {
          this.nodes.rewritePrompt.maxLength = maxPromptLength;
        }
        this.configureRewriteNumber(
          this.nodes.rewriteTemperature,
          this.nodes.rewriteTemperatureRange,
          response.settings && response.settings.temperature,
          false,
        );
        this.configureRewriteNumber(
          this.nodes.rewriteTopK,
          this.nodes.rewriteTopKRange,
          response.settings && response.settings.top_k,
          true,
        );
        this.configureRewriteNumber(
          this.nodes.rewriteTopP,
          this.nodes.rewriteTopPRange,
          response.settings && response.settings.top_p,
          false,
        );
        this.configureRewriteNumber(
          this.nodes.rewriteMinP,
          this.nodes.rewriteMinPRange,
          response.settings && response.settings.min_p,
          false,
        );
        this.configureRewriteNumber(
          this.nodes.rewriteRepeatPenalty,
          this.nodes.rewriteRepeatPenaltyRange,
          response.settings && response.settings.repeat_penalty,
          false,
        );
        this.configureRewriteNumber(
          this.nodes.rewriteMaxTokens,
          this.nodes.rewriteMaxTokensRange,
          response.settings && response.settings.max_tokens,
          true,
        );
        this.renderSelectedRewriteModelStatus();
      } catch (error) {
        this.rewriteModelsResponse = null;
        this.nodes.rewriteModelStatus.textContent =
          "Список моделей недоступен. Проверьте Go API и повторите запрос.";
        this.notifyError(error, "Не удалось получить локальные модели");
      } finally {
        this.busyOperations.delete("rewrite-models");
        this.nodes.rewriteModelRefreshButton.classList.remove("is-busy");
        this.updateRetrySelectedButton();
      }
    }

    configureRewriteNumber(input, hint, setting, integer) {
      const minimum = Number(setting && setting.minimum);
      const maximum = Number(setting && setting.maximum);
      const defaultValue = Number(setting && setting.default);
      input.required = true;
      if (
        !Number.isFinite(minimum) ||
        !Number.isFinite(maximum) ||
        minimum > maximum
      ) {
        input.removeAttribute("min");
        input.removeAttribute("max");
        hint.textContent = "Go API не сообщил допустимый диапазон.";
        if (!input.value && Number.isFinite(defaultValue)) {
          input.value = String(defaultValue);
        }
        return;
      }

      input.min = String(minimum);
      input.max = String(maximum);
      input.step = integer ? "1" : "0.01";
      const current = Number(input.value);
      if (
        !input.value ||
        !Number.isFinite(current) ||
        current < minimum ||
        current > maximum ||
        (integer && !Number.isInteger(current))
      ) {
        input.value = String(
          Number.isFinite(defaultValue) ? defaultValue : minimum,
        );
      }
      hint.textContent =
        `Допустимо: ${minimum}–${maximum}. ` +
        `По умолчанию: ${
          Number.isFinite(defaultValue) ? defaultValue : minimum
        }.`;
    }

    renderSelectedRewriteModelStatus() {
      const response = this.rewriteModelsResponse;
      const model = response && Array.isArray(response.models)
        ? response.models.find(
            (candidate) => candidate.id === this.nodes.rewriteModelSelect.value,
          )
        : null;
      if (!model) {
        this.nodes.rewriteModelStatus.textContent =
          "Выберите модель для локальной правки.";
        return;
      }

      const name = model.display_name || model.id;
      if (model.loaded) {
        this.nodes.rewriteModelStatus.textContent =
          `${name} уже загружена и готова к работе на ` +
          `${model.device || "CPU"}.`;
      } else if (model.available) {
        this.nodes.rewriteModelStatus.textContent =
          `${name} уже находится на сервере. Первый запуск модели на CPU ` +
          "может занять несколько минут.";
      } else {
        const size = this.formatBytes(model.size_bytes);
        this.nodes.rewriteModelStatus.textContent =
          `${name} будет скачана как GGUF${size ? ` (${size})` : ""} при ` +
          "первом запуске. Задача продолжится на сервере даже после " +
          "обновления страницы.";
      }
    }

    async startRewrite(fragmentIDs) {
      if (!this.currentJobID) {
        this.notify("Сначала откройте задачу.", "error");
        return;
      }
      if (
        !this.warningFragments.some(
          (fragment) => fragment.status === "warning",
        )
      ) {
        this.notify("Нет фрагментов для AI-правки.", "error");
        return;
      }
      if (this.isRewriteActive()) {
        this.notify("AI-правка уже выполняется.", "error");
        return;
      }
      if (!this.rewriteModelsResponse) {
        this.notify("Сначала дождитесь списка локальных моделей.", "error");
        this.loadRewriteModels();
        return;
      }

      const warningIDs = new Set(
        this.warningFragments
          .filter((fragment) => fragment.status === "warning")
          .map((fragment) => fragment.id),
      );
      const selectedIDs = fragmentIDs.filter((id) => warningIDs.has(id));
      if (fragmentIDs.length > 0 && selectedIDs.length === 0) {
        this.notify("Выбранные фрагменты уже не требуют проверки.", "error");
        return;
      }

      const modelID = this.nodes.rewriteModelSelect.value;
      const prompt = this.nodes.rewritePrompt.value.trim();
      if (!modelID) {
        this.notify("Выберите модель.", "error");
        this.nodes.rewriteModelSelect.focus();
        return;
      }
      if (!prompt) {
        this.notify("Инструкция модели не может быть пустой.", "error");
        this.nodes.rewritePrompt.focus();
        return;
      }

      const temperature = this.readRewriteNumber(
        this.nodes.rewriteTemperature,
        "Температура",
        this.rewriteModelsResponse.settings &&
          this.rewriteModelsResponse.settings.temperature,
        false,
      );
      if (temperature === null) {
        return;
      }
      const maxTokens = this.readRewriteNumber(
        this.nodes.rewriteMaxTokens,
        "Максимум токенов",
        this.rewriteModelsResponse.settings &&
          this.rewriteModelsResponse.settings.max_tokens,
        true,
      );
      if (maxTokens === null) {
        return;
      }
      const topK = this.readRewriteNumber(
        this.nodes.rewriteTopK,
        "Top K",
        this.rewriteModelsResponse.settings &&
          this.rewriteModelsResponse.settings.top_k,
        true,
      );
      if (topK === null) {
        return;
      }
      const topP = this.readRewriteNumber(
        this.nodes.rewriteTopP,
        "Top P",
        this.rewriteModelsResponse.settings &&
          this.rewriteModelsResponse.settings.top_p,
        false,
      );
      if (topP === null) {
        return;
      }
      const minP = this.readRewriteNumber(
        this.nodes.rewriteMinP,
        "Min P",
        this.rewriteModelsResponse.settings &&
          this.rewriteModelsResponse.settings.min_p,
        false,
      );
      if (minP === null) {
        return;
      }
      const repeatPenalty = this.readRewriteNumber(
        this.nodes.rewriteRepeatPenalty,
        "Repeat penalty",
        this.rewriteModelsResponse.settings &&
          this.rewriteModelsResponse.settings.repeat_penalty,
        false,
      );
      if (repeatPenalty === null) {
        return;
      }

      const button = selectedIDs.length > 0
        ? this.nodes.rewriteSelectedButton
        : this.nodes.rewriteAllButton;
      const jobID = this.currentJobID;
      await this.withBusy("rewrite", button, async () => {
        const response = await this.api.rewriteWarnings(jobID, {
          fragment_ids: selectedIDs,
          model_id: modelID,
          prompt,
          temperature,
          top_k: topK,
          top_p: topP,
          min_p: minP,
          repeat_penalty: repeatPenalty,
          max_tokens: maxTokens,
        });
        const rewriteID =
          response && (response.id || response.rewrite_id);
        if (!rewriteID) {
          throw new Error("API не вернул ID задачи AI-правки");
        }
        this.currentRewrite = {
          ...response,
          id: rewriteID,
          job_id: (response && response.job_id) || jobID,
          status: (response && response.status) || "queued",
        };
        this.persistActiveRewrite(this.currentRewrite, true);
        this.renderRewriteTask(this.currentRewrite);
        this.startRewritePolling(rewriteID);
        this.notify(
          "AI-правка поставлена в очередь. Первый запуск может скачать GGUF " +
          "и несколько минут работать на CPU.",
          "success",
        );
      });
    }

    readRewriteNumber(input, label, setting, integer) {
      const value = Number(input.value);
      const minimum = Number(setting && setting.minimum);
      const maximum = Number(setting && setting.maximum);
      const hasRange =
        Number.isFinite(minimum) &&
        Number.isFinite(maximum) &&
        minimum <= maximum;
      if (
        input.value === "" ||
        !Number.isFinite(value) ||
        (integer && !Number.isInteger(value)) ||
        (hasRange && (value < minimum || value > maximum)) ||
        !input.checkValidity()
      ) {
        const range = hasRange ? ` от ${minimum} до ${maximum}` : "";
        this.notify(`${label}: введите число${range}.`, "error");
        input.focus();
        return null;
      }
      return value;
    }

    resumeStoredRewrite() {
      const raw = this.readStorage(STORAGE.activeRewrite);
      if (!raw) {
        return;
      }
      let stored;
      try {
        stored = JSON.parse(raw);
      } catch (_) {
        this.removeStorage(STORAGE.activeRewrite);
        return;
      }
      if (
        !stored ||
        typeof stored.id !== "string" ||
        !stored.id ||
        stored.id.length > 200 ||
        typeof stored.job_id !== "string" ||
        stored.job_id.length > 200 ||
        REWRITE_TERMINAL_STATUSES.has(stored.status)
      ) {
        this.removeStorage(STORAGE.activeRewrite);
        return;
      }

      this.currentRewrite = {
        id: stored.id,
        job_id: stored.job_id,
        status: stored.status || "queued",
        fragments_count: this.number(stored.fragments_count),
        fragments_pending: this.number(stored.fragments_pending),
        fragments_completed: this.number(stored.fragments_completed),
        fragments_failed: this.number(stored.fragments_failed),
      };
      if (stored.job_id && stored.job_id === this.currentJobID) {
        this.renderRewriteTask(this.currentRewrite);
      } else {
        this.updateRetrySelectedButton();
      }
      this.startRewritePolling(stored.id);
    }

    persistActiveRewrite(task, replace = false) {
      if (!task || typeof task.id !== "string" || !task.id) {
        return;
      }
      if (!replace) {
        const raw = this.readStorage(STORAGE.activeRewrite);
        if (raw) {
          try {
            const existing = JSON.parse(raw);
            if (existing && existing.id && existing.id !== task.id) {
              return;
            }
          } catch (_) {
            // Replace corrupt state with the current verified task.
          }
        }
      }
      this.writeStorage(
        STORAGE.activeRewrite,
        JSON.stringify({
          id: task.id,
          job_id: task.job_id || "",
          status: task.status || "queued",
          fragments_count: this.number(task.fragments_count),
          fragments_pending: this.number(task.fragments_pending),
          fragments_completed: this.number(task.fragments_completed),
          fragments_failed: this.number(task.fragments_failed),
        }),
      );
    }

    clearStoredRewrite(rewriteID) {
      const raw = this.readStorage(STORAGE.activeRewrite);
      if (!raw) {
        return;
      }
      try {
        const stored = JSON.parse(raw);
        if (stored && stored.id && stored.id !== rewriteID) {
          return;
        }
      } catch (_) {
        // Corrupt state is safe to remove.
      }
      this.removeStorage(STORAGE.activeRewrite);
    }

    startRewritePolling(rewriteID) {
      this.rewritePollToken += 1;
      const token = this.rewritePollToken;
      let consecutiveErrors = 0;

      const poll = async () => {
        if (token !== this.rewritePollToken) {
          return;
        }
        if (this.document.hidden) {
          window.setTimeout(poll, JOBS_POLL_INTERVAL_MS);
          return;
        }

        try {
          const task = await this.api.getRewriteTask(rewriteID);
          if (token !== this.rewritePollToken) {
            return;
          }
          this.currentRewrite = task;
          if (!task.job_id || task.job_id === this.currentJobID) {
            this.renderRewriteTask(task);
          } else {
            this.updateRetrySelectedButton();
          }
          consecutiveErrors = 0;

          if (REWRITE_TERMINAL_STATUSES.has(task.status)) {
            this.clearStoredRewrite(rewriteID);
            const taskJobID = task.job_id || "";
            if (task.status === "completed") {
              this.notify("AI-правка всех фрагментов завершена.", "success");
            } else if (task.status === "completed_with_errors") {
              this.notify(
                "AI-правка завершена, но часть фрагментов требует внимания.",
                "error",
              );
            } else {
              this.notify(
                task.error || "AI-правка не выполнена.",
                "error",
              );
            }
            if (taskJobID && taskJobID === this.currentJobID) {
              await this.refreshCurrentJobAndWarnings(taskJobID);
            }
            this.loadRewriteModels(true);
            this.startJobsMonitor();
            return;
          }
          this.persistActiveRewrite(task);
        } catch (error) {
          if (error instanceof APIError && error.status === 404) {
            this.clearStoredRewrite(rewriteID);
            if (
              this.currentRewrite &&
              this.currentRewrite.id === rewriteID
            ) {
              this.currentRewrite = null;
              this.nodes.rewriteProgressRegion.hidden = true;
              this.updateRetrySelectedButton();
            }
            this.notify(
              "Сохранённая задача AI-правки больше не существует.",
              "error",
            );
            return;
          }
          consecutiveErrors += 1;
          this.nodes.rewriteProgressDetails.textContent =
            "Связь с задачей временно потеряна. Повторяем запрос автоматически.";
          if (consecutiveErrors === 1) {
            this.notifyError(error, "Не удалось обновить AI-прогресс");
          }
        }

        if (token === this.rewritePollToken) {
          const delay = consecutiveErrors > 0
            ? Math.min(
                REWRITE_POLL_INTERVAL_MS * (2 ** consecutiveErrors),
                12000,
              )
            : REWRITE_POLL_INTERVAL_MS;
          window.setTimeout(poll, delay);
        }
      };

      poll();
    }

    renderRewriteTask(task) {
      const total = this.number(task.fragments_count);
      const completed = this.number(task.fragments_completed);
      const failed = this.number(task.fragments_failed);
      const derivedPending = Math.max(0, total - completed - failed);
      const pending = Number.isFinite(Number(task.fragments_pending))
        ? this.number(task.fragments_pending)
        : derivedPending;
      const processed = Math.min(total, completed + failed);
      const percentage = total > 0
        ? Math.round((processed / total) * 100)
        : REWRITE_TERMINAL_STATUSES.has(task.status)
          ? 100
          : 0;

      this.nodes.rewriteProgressRegion.hidden = false;
      this.nodes.rewriteProgressLabel.textContent =
        REWRITE_STATUS_LABELS[task.status] ||
        task.status ||
        "Обновляем состояние AI-правки";
      this.nodes.rewriteProgressPercent.textContent = `${percentage}%`;
      this.nodes.rewriteProgressBar.value = percentage;
      this.nodes.rewriteProgressBar.textContent = `${percentage}%`;
      this.nodes.rewriteProgressBar.setAttribute(
        "aria-valuetext",
        `Обработано ${processed} из ${total} фрагментов`,
      );
      this.nodes.rewriteProgressDetails.textContent =
        `Всего: ${total} · ожидают: ${pending} · ` +
        `готово: ${completed} · ошибки: ${failed}.`;

      this.nodes.rewriteFailures.replaceChildren();
      const failedFragments = Array.isArray(task.fragments)
        ? task.fragments.filter((fragment) => fragment.status === "failed")
        : [];
      for (const fragment of failedFragments) {
        const identity = Number.isFinite(Number(fragment.ordinal))
          ? `Фрагмент ${this.number(fragment.ordinal)}`
          : `Фрагмент ${this.shortID(fragment.fragment_id)}`;
        this.nodes.rewriteFailures.append(
          this.createNode(
            "li",
            "",
            `${identity}: ${fragment.error || "причина не указана"}`,
          ),
        );
      }
      if (task.error && failedFragments.length === 0) {
        this.nodes.rewriteFailures.append(
          this.createNode("li", "", task.error),
        );
      }
      this.nodes.rewriteFailures.hidden =
        this.nodes.rewriteFailures.childElementCount === 0;
      this.updateRetrySelectedButton();
    }

    isRewriteActive() {
      return Boolean(
        this.currentRewrite &&
        !REWRITE_TERMINAL_STATUSES.has(this.currentRewrite.status),
      );
    }

    startJobsMonitor(announceError = false) {
      this.jobsPollToken += 1;
      const token = this.jobsPollToken;
      let consecutiveErrors = 0;

      const poll = async () => {
        if (token !== this.jobsPollToken) {
          return;
        }
        if (this.document.hidden) {
          window.setTimeout(poll, JOBS_POLL_INTERVAL_MS);
          return;
        }

        const result = await this.refreshJobs(announceError);
        announceError = false;
        if (token !== this.jobsPollToken) {
          return;
        }

        if (result === true) {
          consecutiveErrors = 0;
        } else if (result === false) {
          consecutiveErrors += 1;
        }
        const delay = result === null
          ? 300
          : consecutiveErrors > 0
            ? Math.min(
                JOBS_POLL_INTERVAL_MS * (2 ** consecutiveErrors),
                30000,
              )
            : JOBS_POLL_INTERVAL_MS;
        window.setTimeout(poll, delay);
      };

      poll();
    }

    async refreshJobs(announceError = false) {
      if (this.jobsRefreshInFlight) {
        return null;
      }

      const requestedFilter = this.jobsFilter;
      const requestedOffset = this.jobsOffset;
      this.jobsRefreshInFlight = true;
      this.nodes.jobsRefreshButton.disabled = true;
      this.nodes.jobsRefreshButton.classList.add("is-busy");
      this.nodes.jobsList.setAttribute("aria-busy", "true");

      try {
        const response = await this.api.listJobs({
          status: requestedFilter,
          limit: this.jobsLimit,
          offset: requestedOffset,
        });
        if (
          requestedFilter !== this.jobsFilter ||
          requestedOffset !== this.jobsOffset
        ) {
          return null;
        }

        const jobs = Array.isArray(response.jobs) ? response.jobs : [];
        this.jobsTotal = this.number(response.total);
        if (jobs.length === 0 && requestedOffset > 0 && this.jobsTotal > 0) {
          this.jobsOffset =
            Math.floor((this.jobsTotal - 1) / this.jobsLimit) * this.jobsLimit;
          return null;
        }

        this.renderJobs(jobs);
        this.nodes.jobsRefreshedAt.textContent =
          `Обновлено ${this.formatDate(new Date())}`;
        return true;
      } catch (error) {
        if (
          requestedFilter !== this.jobsFilter ||
          requestedOffset !== this.jobsOffset
        ) {
          return null;
        }
        this.nodes.jobsSummary.textContent =
          "Не удалось обновить список. Повторим запрос автоматически.";
        this.nodes.jobsRefreshedAt.textContent =
          "Последнее обновление не выполнено";
        if (announceError) {
          this.notifyError(error, "Не удалось обновить список задач");
        }
        return false;
      } finally {
        this.jobsRefreshInFlight = false;
        this.nodes.jobsRefreshButton.disabled = false;
        this.nodes.jobsRefreshButton.classList.remove("is-busy");
        this.nodes.jobsList.setAttribute("aria-busy", "false");
      }
    }

    renderJobs(jobs) {
      this.nodes.jobsList.replaceChildren();

      if (jobs.length === 0) {
        const empty = this.createNode("div", "jobs-empty");
        empty.setAttribute("role", "listitem");
        const symbol = this.createNode("span", "", "✓");
        symbol.setAttribute("aria-hidden", "true");
        const title = this.createNode(
          "h3",
          "",
          this.jobsFilter === "active"
            ? "Активных задач сейчас нет"
            : "По этому фильтру задач нет",
        );
        const description = this.createNode(
          "p",
          "",
          this.jobsFilter === "active"
            ? "Новая генерация появится здесь сразу после запуска."
            : "Выберите другой статус или обновите список позже.",
        );
        empty.append(symbol, title, description);
        this.nodes.jobsList.append(empty);
      } else {
        for (const job of jobs) {
          this.nodes.jobsList.append(this.createJobCard(job));
        }
      }

      const first = this.jobsTotal === 0 ? 0 : this.jobsOffset + 1;
      const last = Math.min(
        this.jobsTotal,
        this.jobsOffset + jobs.length,
      );
      this.nodes.jobsSummary.textContent = this.jobsTotal === 0
        ? "Задач не найдено."
        : `Показаны задачи ${first}–${last} из ${this.jobsTotal}.`;

      const pageCount = Math.max(
        1,
        Math.ceil(this.jobsTotal / this.jobsLimit),
      );
      const pageNumber = Math.min(
        pageCount,
        Math.floor(this.jobsOffset / this.jobsLimit) + 1,
      );
      this.nodes.jobsPageLabel.textContent =
        `Страница ${pageNumber} из ${pageCount}`;
      this.nodes.jobsPrevButton.disabled = this.jobsOffset === 0;
      this.nodes.jobsNextButton.disabled =
        this.jobsOffset + jobs.length >= this.jobsTotal;
    }

    createJobCard(job) {
      const jobID = typeof job.id === "string" ? job.id : "";
      const card = this.createNode("article", "jobs-card");
      card.setAttribute("role", "listitem");
      if (jobID && jobID === this.currentJobID) {
        card.classList.add("jobs-card--current");
        card.setAttribute("aria-current", "true");
      }

      const header = this.createNode("div", "jobs-card-header");
      const status = this.createNode(
        "span",
        "status-badge",
        JOB_STATUS_LABELS[job.status] || job.status || "Неизвестно",
      );
      status.dataset.status = job.status || "";
      const updated = this.createNode(
        "time",
        "jobs-card-updated",
        job.updated_at
          ? `обновлено ${this.formatDate(job.updated_at)}`
          : "время не указано",
      );
      if (job.updated_at) {
        updated.dateTime = job.updated_at;
      }
      header.append(status, updated);

      const book = this.books.find((item) => item.id === job.book_id);
      const title = this.createNode(
        "h3",
        "",
        book
          ? book.title || "Книга без названия"
          : `Книга ${this.shortID(job.book_id)}`,
      );
      const identity = this.createNode("p", "jobs-card-identity");
      const idLabel = this.createNode("span", "", "Задача");
      const id = this.createNode("code", "", jobID || "ID не указан");
      identity.append(idLabel, id);

      const total = this.number(job.fragments_count);
      const pending = this.number(job.fragments_pending);
      const ready = this.number(job.fragments_ready);
      const warnings = this.number(job.fragments_warnings);
      const failed = this.number(job.fragments_failed);
      const completed = Math.min(total, ready + warnings + failed);
      const percentage = total > 0
        ? Math.round((completed / total) * 100)
        : 0;

      const progressRow = this.createNode("div", "jobs-card-progress");
      const progress = this.createNode("progress", "progress-track");
      progress.max = 100;
      progress.value = percentage;
      progress.textContent = `${percentage}%`;
      progress.setAttribute(
        "aria-label",
        `Прогресс задачи ${jobID || "без ID"}: ${percentage} процентов`,
      );
      const progressValue = this.createNode(
        "strong",
        "",
        `${percentage}%`,
      );
      progressRow.append(progress, progressValue);

      const counters = this.createNode("dl", "jobs-card-counters");
      for (const [label, value, modifier] of [
        ["Всего", total, "all"],
        ["Ожидают", pending, "pending"],
        ["Готово", ready, "ready"],
        ["Проверить", warnings, "warning"],
        ["Ошибки", failed, "failed"],
      ]) {
        const counter = this.createNode(
          "div",
          `jobs-mini-counter jobs-mini-counter--${modifier}`,
        );
        counter.append(
          this.createNode("dt", "", label),
          this.createNode("dd", "", String(value)),
        );
        counters.append(counter);
      }

      const footer = this.createNode("div", "jobs-card-footer");
      const context = this.createNode(
        "span",
        "",
        `Голос ${this.shortID(job.voice_id)}`,
      );
      const openButton = this.createNode(
        "button",
        "button button--secondary",
        "Открыть подробно",
      );
      openButton.type = "button";
      openButton.disabled = !jobID;
      openButton.setAttribute(
        "aria-label",
        `Открыть подробности задачи ${jobID || "без ID"}`,
      );
      openButton.addEventListener("click", () => {
        this.withBusy(`open-job-${jobID}`, openButton, async () => {
          await this.openJob(jobID, true);
          this.byID("job-title").scrollIntoView({
            behavior: "smooth",
            block: "start",
          });
        });
      });
      footer.append(context, openButton);

      card.append(header, title, identity, progressRow, counters, footer);
      return card;
    }

    renderBooks(preferredBookID = "") {
      const previous = preferredBookID || this.nodes.bookSelect.value;
      this.nodes.bookSelect.replaceChildren();

      if (this.books.length === 0) {
        this.nodes.bookSelect.append(
          new Option("Сначала загрузите книгу", ""),
        );
        this.nodes.bookSelect.disabled = true;
      } else {
        this.nodes.bookSelect.disabled = false;
        for (const book of this.books) {
          const title = book.title || "Без названия";
          this.nodes.bookSelect.append(
            new Option(`${title} · ${this.number(book.fragments_count)} фр.`, book.id),
          );
        }
        const exists = this.books.some((book) => book.id === previous);
        this.nodes.bookSelect.value = exists ? previous : this.books[0].id;
      }

      this.renderBookSummary();
      this.renderSelectionSummary();
    }

    renderBookSummary() {
      const book = this.selectedBook();
      if (!book) {
        this.nodes.bookSummary.textContent =
          "Здесь появятся название, автор и количество фрагментов.";
        return;
      }
      const authors = Array.isArray(book.authors) && book.authors.length
        ? book.authors.join(", ")
        : "автор не указан";
      this.nodes.bookSummary.textContent =
        `${authors} · ${this.number(book.chapters_count)} гл. · ` +
        `${this.number(book.fragments_count)} фр.`;
    }

    renderVoices(preferredVoiceID = "") {
      const previous = preferredVoiceID || this.nodes.voiceSelect.value;
      this.nodes.voiceSelect.replaceChildren();
      this.nodes.voiceList.replaceChildren();

      if (this.voices.length === 0) {
        this.nodes.voiceSelect.append(
          new Option("Сначала создайте голос", ""),
        );
        this.nodes.voiceSelect.disabled = true;
      } else {
        this.nodes.voiceSelect.disabled = false;
        for (const voice of this.voices) {
          const title = voice.name || "Без названия";
          this.nodes.voiceSelect.append(new Option(title, voice.id));

          const chip = this.createNode("span", "voice-chip");
          const chipLabel = this.createNode("span", "", title);
          chip.title = `${title} · ${String(voice.format || "").toUpperCase()}`;
          chip.append(chipLabel);
          this.nodes.voiceList.append(chip);
        }
        const exists = this.voices.some((voice) => voice.id === previous);
        this.nodes.voiceSelect.value = exists ? previous : this.voices[0].id;
      }

      this.renderSelectionSummary();
    }

    renderSelectionSummary() {
      const book = this.selectedBook();
      const voice = this.selectedVoice();
      this.nodes.selectedBookLabel.textContent = book
        ? book.title || book.id
        : "не выбрана";
      this.nodes.selectedVoiceLabel.textContent = voice
        ? voice.name || voice.id
        : "не выбран";
      const activeJob = Boolean(
        this.currentJob &&
        !TERMINAL_JOB_STATUSES.has(this.currentJob.status),
      );
      this.nodes.generateButton.disabled =
        !book ||
        !voice ||
        this.busyOperations.has("generate") ||
        activeJob;
    }

    renderJob(job) {
      this.nodes.jobEmpty.hidden = true;
      this.nodes.jobDashboard.hidden = false;

      const total = this.number(job.fragments_count);
      const pending = this.number(job.fragments_pending);
      const ready = this.number(job.fragments_ready);
      const warnings = this.number(job.fragments_warnings);
      const failed = this.number(job.fragments_failed);
      const completed = Math.min(total, ready + warnings + failed);
      const percentage = total > 0
        ? Math.round((completed / total) * 100)
        : 0;

      this.nodes.jobStatusBadge.textContent =
        JOB_STATUS_LABELS[job.status] || job.status || "Неизвестно";
      this.nodes.jobStatusBadge.dataset.status = job.status || "";
      this.nodes.jobIDLabel.textContent = job.id || this.currentJobID;
      this.nodes.jobIDInput.value = job.id || this.currentJobID;
      this.nodes.progressBar.value = percentage;
      this.nodes.progressBar.textContent = `${percentage}%`;
      this.nodes.progressBar.setAttribute(
        "aria-valuetext",
        `Выполнено ${percentage} процентов`,
      );
      this.nodes.progressLabel.textContent = `${percentage}%`;
      this.nodes.jobUpdatedLabel.textContent = job.updated_at
        ? `Обновлено ${this.formatDate(job.updated_at)}`
        : "Состояние обновлено";
      this.nodes.counterTotal.textContent = String(total);
      this.nodes.counterPending.textContent = String(pending);
      this.nodes.counterReady.textContent = String(ready);
      this.nodes.counterWarnings.textContent = String(warnings);
      this.nodes.counterFailed.textContent = String(failed);
      const snapshot =
        job.generation_settings &&
        typeof job.generation_settings === "object"
          ? job.generation_settings
          : null;
      this.nodes.jobSettingsSnapshot.textContent = snapshot
        ? JSON.stringify(snapshot, null, 2)
        : "Для этой задачи снимок настроек недоступен.";

      const canDownload = job.status === "completed" ||
        job.status === "completed_with_warnings";
      if (canDownload) {
        this.nodes.downloadLink.href =
          `/v1/job/${encodeURIComponent(job.id)}/audio.zip`;
        this.nodes.downloadLink.download =
          `book-${job.book_id || "audio"}-chapters-flac.zip`;
        this.nodes.downloadLink.classList.remove("is-disabled");
        this.nodes.downloadLink.setAttribute("aria-disabled", "false");
        this.nodes.downloadLink.setAttribute("tabindex", "0");
        this.nodes.chapterDownloads.hidden = false;
        this.loadChapterDownloads(job);
      } else {
        this.nodes.downloadLink.removeAttribute("href");
        this.nodes.downloadLink.removeAttribute("download");
        this.nodes.downloadLink.classList.add("is-disabled");
        this.nodes.downloadLink.setAttribute("aria-disabled", "true");
        this.nodes.downloadLink.setAttribute("tabindex", "-1");
        this.nodes.chapterDownloads.hidden = false;
      }
      this.renderSelectionSummary();
    }

    async loadChapterDownloads(job) {
      if (
        !job ||
        job.status !== "completed" ||
        !job.id ||
        this.chapterDataJobID === job.id ||
        this.chapterLoadingJobID === job.id
      ) {
        return;
      }

      this.chapterLoadingJobID = job.id;
      this.chapterRequestToken += 1;
      const requestToken = this.chapterRequestToken;
      this.nodes.chapterDownloads.hidden = false;
      this.nodes.chapterDownloadsStatus.textContent =
        "Загружаем готовые главы FLAC…";
      this.nodes.chapterDownloadsList.replaceChildren();

      try {
        const response = await this.api.listChapters(job.id);
        if (
          requestToken !== this.chapterRequestToken ||
          this.currentJobID !== job.id
        ) {
          return;
        }
        const chapters = Array.isArray(response.chapters)
          ? response.chapters
          : [];
        const fragments = Array.isArray(response.fragments)
          ? response.fragments
          : [];
        this.renderChapterDownloads(job, chapters);
        this.renderFragments(fragments);
        this.chapterDataJobID = job.id;
      } catch (error) {
        if (requestToken !== this.chapterRequestToken) {
          return;
        }
        this.nodes.chapterDownloadsStatus.textContent =
          "Не удалось загрузить главы FLAC. ZIP всей книги остаётся доступен.";
        this.notifyError(error, "Не удалось загрузить главы FLAC");
      } finally {
        if (this.chapterLoadingJobID === job.id) {
          this.chapterLoadingJobID = "";
        }
      }
    }

    renderChapterDownloads(job, chapters) {
      this.nodes.chapterDownloadsList.replaceChildren();
      const normalized = chapters
        .map((chapter) => {
          const rawNumber = chapter.chapter_number ?? chapter.number;
          const number = Number(rawNumber);
          if (!Number.isInteger(number) || number <= 0) return null;
          return { ...chapter, chapter_number: number };
        })
        .filter(Boolean)
        .sort((left, right) => left.chapter_number - right.chapter_number);
      if (normalized.length === 0) {
        this.nodes.chapterDownloadsStatus.textContent = "Метаданные глав ещё не появились.";
        return;
      }
      const audioComplete = normalized.filter((chapter) => chapter.audio_complete || chapter.ready).length;
      const reviewRequired = normalized.reduce(
        (sum, chapter) => sum + this.number(chapter.review_required),
        0,
      );
      this.nodes.chapterDownloadsStatus.textContent =
        `Полностью озвучено: ${audioComplete} из ${normalized.length}. ` +
        (reviewRequired > 0
          ? `Замечаний для проверки: ${reviewRequired}. Они не блокируют скачивание готового аудио.`
          : "Замечаний для проверки нет.");

      for (const chapter of normalized) {
        const item = this.createNode("article", "chapter-download-link chapter-overview-card");
        const number = this.createNode(
          "span", "chapter-number", String(chapter.chapter_number).padStart(2, "0"),
        );
        number.setAttribute("aria-hidden", "true");
        const copy = this.createNode("span", "chapter-download-copy");
        const title = this.createNode(
          "strong", "", chapter.title || `Глава ${chapter.chapter_number}`,
        );
        const voiced = this.number(chapter.fragments_voiced ?? chapter.fragments_ready);
        const total = this.number(chapter.fragments_count);
        const review = this.number(chapter.review_required);
        copy.append(
          title,
          this.createNode(
            "small", "",
            `озвучено ${voiced} из ${total}` +
              (review > 0 ? ` · проверить ${review}` : " · без замечаний"),
          ),
        );
        const actions = this.createNode("span", "chapter-card-actions");
        const reviewLink = this.createNode("a", "chapter-review-link", "Открыть главу");
        reviewLink.href = chapter.review_url ||
          `/jobs/${encodeURIComponent(job.id)}/chapters/${chapter.chapter_number}`;
        reviewLink.setAttribute("aria-label", `Открыть проверку главы ${chapter.chapter_number}`);
        actions.append(reviewLink);
        const ready = Boolean((chapter.audio_complete || chapter.ready) && chapter.audio_url);
        if (ready) {
          const download = this.createNode("a", "chapter-flac-link", "Скачать FLAC");
          download.href = chapter.audio_url;
          download.download = chapter.audio_filename || this.chapterFLACFilename(chapter);
          download.setAttribute("aria-label", `Скачать главу ${chapter.chapter_number} как FLAC`);
          actions.append(download);
        } else {
          actions.append(this.createNode("span", "chapter-pending-label", "Аудио ещё создаётся"));
        }
        item.append(number, copy, actions);
        this.nodes.chapterDownloadsList.append(item);
      }
    }

    resetChapterDownloads() {
      if (
        this.nodes.chapterDownloads.hidden &&
        !this.chapterDataJobID &&
        !this.chapterLoadingJobID
      ) {
        return;
      }
      this.chapterRequestToken += 1;
      this.chapterDataJobID = "";
      this.chapterLoadingJobID = "";
      this.nodes.chapterDownloads.hidden = true;
      this.nodes.chapterDownloadsStatus.textContent =
        "Загружаем готовые главы FLAC…";
      this.nodes.chapterDownloadsList.replaceChildren();
    }

    renderWarnings(fragments) {
      this.renderFragments(fragments);
    }

    renderFragments(_fragments) {
      // Fragment cards intentionally live on the dedicated per-chapter page.
      // Keeping this method as a no-op preserves existing internal call sites
      // without putting hundreds of textareas/audio elements on the dashboard.
      this.warningFragments = [];
      this.selectedWarningIDs.clear();
      this.nodes.warningsList.replaceChildren();
      this.nodes.warningsSection.hidden = true;
      this.nodes.warningsSummary.textContent = "";
      this.updateRetrySelectedButton();
    }

    createProgressFragmentCard(fragment) {
      const card = this.createNode("article", "warning-card fragment-progress-card");
      card.dataset.fragmentId = fragment.id;
      const header = this.createNode("div", "warning-card-header");
      const status = this.createNode(
        "span",
        "warning-code",
        fragment.status === "ready"
          ? "Готов"
          : fragment.status === "generating"
            ? "Озвучивается"
            : "Ожидает",
      );
      const meta = this.createNode(
        "span",
        "fragment-meta",
        `Глава ${this.number(fragment.chapter_number)} · ` +
          `фрагмент ${this.number(fragment.ordinal)} · ` +
          `попытка ${this.number(fragment.attempt)}`,
      );
      header.append(status, meta);

      const body = this.createNode("div", "warning-columns");
      const source = this.createNode("div", "warning-block");
      source.append(this.createNode("h4", "", "Текст для озвучивания"));
      const textarea = this.createNode("textarea");
      textarea.value = fragment.text || "";
      textarea.maxLength = 20000;
      textarea.readOnly = !fragment.editable;
      textarea.setAttribute(
        "aria-label",
        `Текст фрагмента ${this.number(fragment.ordinal)}`,
      );
      source.append(textarea);

      const preview = this.createNode("div", "warning-block");
      preview.append(this.createNode("h4", "", "Прослушивание"));
      if (fragment.audio_available) {
        const audio = this.createNode("audio");
        audio.controls = true;
        audio.preload = "none";
        audio.src = `/v1/fragment/${encodeURIComponent(fragment.id)}/audio.wav`;
        audio.setAttribute(
          "aria-label",
          `Аудио фрагмента ${this.number(fragment.ordinal)}`,
        );
        preview.append(
          audio,
          this.createNode(
            "small",
            "audio-status",
            "Аудио загружается только после нажатия воспроизведения.",
          ),
        );
      } else {
        preview.append(
          this.createNode(
            "p",
            "text-change-note",
            fragment.status === "generating"
              ? "Фрагмент сейчас озвучивается."
              : "Аудио ещё не готово.",
          ),
        );
      }
      body.append(source, preview);

      const footer = this.createNode("div", "warning-card-footer");
      footer.append(
        this.createNode(
          "span",
          "fragment-meta",
          fragment.duration_ms
            ? `Длительность: ${this.number(fragment.duration_ms)} мс`
            : "Длительность появится после генерации",
        ),
      );
      if (fragment.editable) {
        const save = this.createNode(
          "button",
          "button button--quiet",
          "Сохранить и переозвучить",
        );
        save.type = "button";
        save.addEventListener("click", () => {
          this.editWarning(fragment, textarea, save);
        });
        footer.append(save);
      }

      const revisions = this.createNode("details", "revision-history");
      const revisionsSummary = this.createNode("summary", "", "История версий текста");
      const revisionsContent = this.createNode("div", "revision-history-content");
      revisionsContent.textContent =
        "Откройте историю, чтобы выбрать и просмотреть редакцию.";
      revisions.addEventListener("toggle", () => {
        if (revisions.open) {
          this.expandedRevisionIDs.add(fragment.id);
          this.loadRevisionHistory(fragment, revisionsContent);
        } else {
          this.expandedRevisionIDs.delete(fragment.id);
        }
      });
      revisions.append(revisionsSummary, revisionsContent);
      card.append(header, body, footer, revisions);
      return card;
    }

    createWarningCard(fragment) {
      const card = this.createNode("article", "warning-card");
      card.dataset.fragmentId = fragment.id;

      const header = this.createNode("div", "warning-card-header");
      const selector = this.createNode("label", "warning-selector");
      const checkbox = this.createNode("input");
      checkbox.type = "checkbox";
      checkbox.id = `warning-select-${this.safeDOMID(fragment.id)}`;
      checkbox.value = fragment.id;
      checkbox.checked = this.selectedWarningIDs.has(fragment.id);
      checkbox.setAttribute(
        "aria-label",
        `Выбрать фрагмент ${this.number(fragment.ordinal)}`,
      );
      checkbox.addEventListener("change", () => {
        if (checkbox.checked) {
          this.selectedWarningIDs.add(fragment.id);
        } else {
          this.selectedWarningIDs.delete(fragment.id);
        }
        this.updateRetrySelectedButton();
      });
      selector.append(checkbox, this.document.createTextNode("Выбрать"));

      const meta = this.createNode(
        "span",
        "fragment-meta",
        `Глава ${this.number(fragment.chapter_number)} · ` +
          `фрагмент ${this.number(fragment.ordinal)} · ` +
          `попытка ${this.number(fragment.attempt)}`,
      );
      header.append(selector, meta);

      const textOnlyChange = TEXT_ONLY_WARNING_CODES.has(
        fragment.warning_code,
      );
      const audioReviewAvailable =
        fragment.status === "warning" && !textOnlyChange;
      const review = this.createNode(
        "section",
        audioReviewAvailable
          ? "manual-review"
          : "manual-review manual-review--text-only",
      );
      const audioGroup = this.createNode("div", "warning-audio");
      const audioHeading = this.createNode("h4", "", "Аудио фрагмента");
      const audio = this.createNode("audio");
      audio.controls = true;
      audio.preload = "none";
      audio.src =
        `/v1/fragment/${encodeURIComponent(fragment.id)}/audio.wav`;
      audio.setAttribute(
        "aria-label",
        `Аудио фрагмента ${this.number(fragment.ordinal)}`,
      );
      const audioStatus = this.createNode(
        "small",
        "audio-status",
        "Аудио загрузится только после нажатия воспроизведения.",
      );
      audio.addEventListener("error", () => {
        audioStatus.textContent =
          "Не удалось загрузить аудио. Оно могло быть удалено или ещё не готово.";
      });
      audioGroup.append(audioHeading, audio, audioStatus);

      const approvalGroup = this.createNode("div", "manual-approval");
      const reasonLabel = this.createNode(
        "label",
        "",
        "Причина ручного подтверждения",
      );
      const reason = this.createNode("textarea");
      reason.rows = 2;
      reason.maxLength = 2000;
      reason.placeholder =
        "Например: произношение корректно, различие вызвано пунктуацией";
      reason.setAttribute(
        "aria-label",
        `Причина подтверждения фрагмента ${this.number(fragment.ordinal)}`,
      );
      const approveButton = this.createNode(
        "button",
        "button button--secondary",
        "Подтвердить вручную",
      );
      approveButton.type = "button";
      approveButton.addEventListener("click", () => {
        this.approveWarning(fragment, reason, approveButton);
      });
      reasonLabel.append(reason);
      approvalGroup.append(reasonLabel, approveButton);
      if (!audioReviewAvailable) {
        const unavailableMessage = textOnlyChange
          ? "Это маркер изменения текста. Новое аудио появится после " +
            "повторной генерации, поэтому прослушивание и ручное " +
            "подтверждение пока недоступны."
          : "Генерация фрагмента завершилась ошибкой, поэтому готового " +
            "аудио для прослушивания и подтверждения нет. Запустите повтор.";
        review.append(
          this.createNode(
            "p",
            "text-change-note",
            unavailableMessage,
          ),
        );
      } else {
        review.append(audioGroup, approvalGroup);
      }

      const columns = this.createNode("div", "warning-columns");
      const sourceBlock = this.createNode("div", "warning-block");
      const sourceHeading = this.createNode("h4", "", "Текст для озвучивания");
      const textarea = this.createNode("textarea");
      textarea.value = fragment.text || "";
      textarea.maxLength = 20000;
      textarea.setAttribute(
        "aria-label",
        `Текст фрагмента ${this.number(fragment.ordinal)}`,
      );
      sourceBlock.append(sourceHeading, textarea);

      const transcriptBlock = this.createNode("div", "warning-block");
      const transcriptHeading = this.createNode(
        "h4",
        "",
        "Результат распознавания",
      );
      const transcript = this.createNode(
        "pre",
        "transcript",
        fragment.stt_text || fragment.error || "Распознавание не получено.",
      );
      transcriptBlock.append(transcriptHeading, transcript);
      columns.append(sourceBlock, transcriptBlock);

      const footer = this.createNode("div", "warning-card-footer");
      const warningCode = this.createNode(
        "span",
        "warning-code",
        WARNING_LABELS[fragment.warning_code] ||
          fragment.warning_code ||
          fragment.error ||
          "требуется проверка",
      );
      const saveButton = this.createNode(
        "button",
        "button button--quiet",
        "Сохранить правку",
      );
      saveButton.type = "button";
      saveButton.addEventListener("click", () => {
        this.editWarning(fragment, textarea, saveButton);
      });
      footer.append(warningCode, saveButton);

      const revisions = this.createNode("details", "revision-history");
      const revisionsSummary = this.createNode(
        "summary",
        "",
        "История версий текста",
      );
      const revisionsContent = this.createNode(
        "div",
        "revision-history-content",
      );
      revisionsContent.textContent =
        "Откройте историю, чтобы выбрать и просмотреть редакцию.";
      revisions.addEventListener("toggle", () => {
        if (revisions.open) {
          this.expandedRevisionIDs.add(fragment.id);
          this.loadRevisionHistory(fragment, revisionsContent);
        } else {
          this.expandedRevisionIDs.delete(fragment.id);
        }
      });
      revisions.append(revisionsSummary, revisionsContent);

      card.append(header, review, columns, footer, revisions);
      return card;
    }

    async approveWarning(fragment, reasonInput, button) {
      if (this.isRewriteActive()) {
        this.notify(
          "Дождитесь завершения AI-правки перед подтверждением фрагмента.",
          "error",
        );
        return;
      }
      const reason = reasonInput.value.trim();
      if (!reason) {
        this.notify("Укажите причину ручного подтверждения.", "error");
        reasonInput.focus();
        return;
      }

      const jobID = this.currentJobID;
      await this.withBusy(
        `approve-${fragment.id}`,
        button,
        async () => {
          const response = await this.api.approveFragment(fragment.id, reason);
          if (jobID !== this.currentJobID) {
            return;
          }
          this.expandedRevisionIDs.delete(fragment.id);
          if (response && response.job) {
            this.currentJob = response.job;
            this.renderJob(response.job);
          }
          await this.refreshCurrentJobAndWarnings(jobID);
          this.startJobsMonitor();
          this.notify(
            "Фрагмент подтверждён и больше не требует проверки.",
            "success",
          );
        },
      );
    }

    async loadRevisionHistory(fragment, container) {
      if (
        container.dataset.loaded === "true" ||
        container.dataset.loading === "true"
      ) {
        return;
      }
      container.dataset.loading = "true";
      container.setAttribute("aria-busy", "true");
      container.replaceChildren(
        this.createNode("p", "revision-loading", "Загружаем версии…"),
      );

      try {
        const response = await this.api.getFragmentRevisions(fragment.id);
        if (!container.isConnected) {
          return;
        }
        this.renderRevisionHistory(fragment, response, container);
        container.dataset.loaded = "true";
      } catch (error) {
        if (!container.isConnected) {
          return;
        }
        const message = this.createNode(
          "p",
          "revision-error",
          "Историю версий загрузить не удалось.",
        );
        const retry = this.createNode(
          "button",
          "button button--quiet",
          "Повторить",
        );
        retry.type = "button";
        retry.addEventListener("click", () => {
          container.dataset.loaded = "false";
          this.loadRevisionHistory(fragment, container);
        });
        container.replaceChildren(message, retry);
        this.notifyError(error, "Не удалось загрузить историю версий");
      } finally {
        container.dataset.loading = "false";
        container.setAttribute("aria-busy", "false");
      }
    }

    renderRevisionHistory(fragment, response, container) {
      const revisions = Array.isArray(response && response.revisions)
        ? [...response.revisions].sort(
            (left, right) =>
              this.number(right.revision_number) -
              this.number(left.revision_number),
          )
        : [];
      container.replaceChildren();

      if (revisions.length === 0) {
        container.append(
          this.createNode("p", "revision-empty", "Сохранённых версий пока нет."),
        );
        return;
      }

      const currentRevisionID =
        (response && response.current_revision_id) ||
        fragment.current_revision_id ||
        "";
      const selectID = `revision-select-${this.safeDOMID(fragment.id)}`;
      const field = this.createNode("label", "revision-select-field");
      const fieldLabel = this.createNode("span", "", "Версия для просмотра");
      const select = this.createNode("select");
      select.id = selectID;
      for (const revision of revisions) {
        const number = this.number(revision.revision_number);
        const source =
          REVISION_SOURCE_LABELS[revision.source] ||
          revision.source ||
          "Версия";
        const current = revision.id === currentRevisionID ? " · текущая" : "";
        select.append(
          new Option(`Версия ${number} · ${source}${current}`, revision.id),
        );
      }
      field.append(fieldLabel, select);

      const preview = this.createNode("pre", "revision-preview");
      preview.setAttribute("tabindex", "0");
      const metadata = this.createNode("p", "revision-metadata");
      const reasonLabel = this.createNode(
        "label",
        "revision-restore-reason",
        "Причина восстановления",
      );
      const restoreReason = this.createNode("textarea");
      restoreReason.rows = 2;
      restoreReason.maxLength = 2000;
      restoreReason.placeholder =
        "Почему эта версия должна снова стать текущей";
      reasonLabel.append(restoreReason);
      const restoreButton = this.createNode(
        "button",
        "button button--secondary",
        "Восстановить эту версию",
      );
      restoreButton.type = "button";

      const selectedRevision = () =>
        revisions.find((revision) => revision.id === select.value) || null;
      const renderSelection = () => {
        const revision = selectedRevision();
        if (!revision) {
          preview.textContent = "Версия недоступна.";
          metadata.textContent = "";
          restoreButton.disabled = true;
          return;
        }
        preview.textContent = revision.text || "";
        const source =
          REVISION_SOURCE_LABELS[revision.source] ||
          revision.source ||
          "Версия";
        const parts = [
          source,
          revision.created_at
            ? this.formatLongDate(revision.created_at)
            : "время не указано",
        ];
        if (revision.model_id) {
          parts.push(`модель ${revision.model_id}`);
        }
        if (revision.reason) {
          parts.push(`причина: ${revision.reason}`);
        }
        metadata.textContent = parts.join(" · ");
        restoreButton.disabled =
          revision.id === currentRevisionID ||
          this.busyOperations.has(`restore-${fragment.id}`);
        restoreButton.textContent =
          revision.id === currentRevisionID
            ? "Это текущая версия"
            : "Восстановить эту версию";
      };
      select.addEventListener("change", renderSelection);
      restoreButton.addEventListener("click", () => {
        const revision = selectedRevision();
        if (!revision || revision.id === currentRevisionID) {
          return;
        }
        this.restoreRevision(
          fragment,
          revision,
          restoreReason,
          restoreButton,
        );
      });
      renderSelection();
      container.append(
        field,
        preview,
        metadata,
        reasonLabel,
        restoreButton,
      );
    }

    async restoreRevision(fragment, revision, reasonInput, button) {
      if (this.isRewriteActive()) {
        this.notify(
          "Дождитесь завершения AI-правки перед восстановлением версии.",
          "error",
        );
        return;
      }
      const reason = reasonInput.value.trim();
      if (!reason) {
        this.notify("Укажите причину восстановления версии.", "error");
        reasonInput.focus();
        return;
      }
      const jobID = this.currentJobID;
      await this.withBusy(
        `restore-${fragment.id}`,
        button,
        async () => {
          await this.api.restoreFragmentRevision(
            fragment.id,
            revision.id,
            reason,
          );
          if (jobID !== this.currentJobID) {
            return;
          }
          await this.refreshCurrentJobAndWarnings(jobID);
          this.startJobsMonitor();
          this.notify("Выбранная версия восстановлена.", "success");
        },
      );
    }

    async refreshCurrentJobAndWarnings(jobID) {
      if (!jobID || jobID !== this.currentJobID) {
        return;
      }
      try {
        const job = await this.api.getJob(jobID);
        if (jobID !== this.currentJobID) {
          return;
        }
        this.currentJob = job;
        this.renderJob(job);
      } catch (error) {
        this.notifyError(error, "Не удалось обновить состояние задачи");
      }
      await this.loadWarnings(jobID);
    }

    updateRetrySelectedButton() {
      const count = this.selectedWarningIDs.size;
      const rewriteableIDs = new Set(
        this.warningFragments
          .filter((fragment) => fragment.status === "warning")
          .map((fragment) => fragment.id),
      );
      const rewriteCount = Array.from(this.selectedWarningIDs).filter(
        (id) => rewriteableIDs.has(id),
      ).length;
      const rewriteActive = this.isRewriteActive();
      const retryActive = this.busyOperations.has("retry");
      this.nodes.retrySelectedButton.disabled =
        count === 0 || retryActive || rewriteActive;
      this.nodes.retrySelectedButton.textContent =
        count > 0 ? `Повторить выбранные (${count})` : "Повторить выбранные";
      this.nodes.retryAllButton.disabled =
        this.warningFragments.length === 0 || retryActive || rewriteActive;

      const models = this.rewriteModelsResponse;
      const hasModels = Boolean(
        models && Array.isArray(models.models) && models.models.length > 0,
      );
      const rewriteSubmitting = this.busyOperations.has("rewrite");
      const controlsDisabled =
        !hasModels || rewriteActive || rewriteSubmitting;
      this.nodes.rewriteModelSelect.disabled = controlsDisabled;
      this.nodes.rewritePrompt.disabled = controlsDisabled;
      this.nodes.rewriteTemperature.disabled = controlsDisabled;
      this.nodes.rewriteTopK.disabled = controlsDisabled;
      this.nodes.rewriteTopP.disabled = controlsDisabled;
      this.nodes.rewriteMinP.disabled = controlsDisabled;
      this.nodes.rewriteRepeatPenalty.disabled = controlsDisabled;
      this.nodes.rewriteMaxTokens.disabled = controlsDisabled;
      this.nodes.rewritePromptResetButton.disabled = controlsDisabled;
      this.nodes.rewriteSelectedButton.disabled =
        rewriteCount === 0 || controlsDisabled;
      this.nodes.rewriteSelectedButton.textContent =
        rewriteCount > 0
          ? `Переписать выбранные (${rewriteCount})`
          : "Переписать выбранные";
      this.nodes.rewriteAllButton.disabled =
        rewriteableIDs.size === 0 || controlsDisabled;
      this.nodes.rewriteModelRefreshButton.disabled =
        this.busyOperations.has("rewrite-models") || rewriteActive;
    }

    selectedBook() {
      const id = this.nodes.bookSelect.value;
      return this.books.find((book) => book.id === id) || null;
    }

    selectedVoice() {
      const id = this.nodes.voiceSelect.value;
      return this.voices.find((voice) => voice.id === id) || null;
    }

    upsertBook(book) {
      if (!book || typeof book.id !== "string" || !book.id) {
        return;
      }
      this.books = [
        book,
        ...this.books.filter((existing) => existing.id !== book.id),
      ].slice(0, 30);
      this.writeStorage(STORAGE.books, JSON.stringify(this.books));
    }

    async ensureBookMetadata(bookID) {
      if (!bookID || this.books.some((book) => book.id === bookID)) {
        return;
      }
      try {
        const book = await this.api.getBook(bookID);
        this.upsertBook(book);
        this.renderBooks(bookID);
      } catch (_) {
        // Job progress remains usable even when its book metadata is unavailable.
      }
    }

    loadBooks() {
      const value = this.readStorage(STORAGE.books);
      if (!value) {
        return [];
      }
      try {
        const books = JSON.parse(value);
        if (!Array.isArray(books)) {
          return [];
        }
        return books
          .filter(
            (book) =>
              book &&
              typeof book.id === "string" &&
              book.id.length > 0 &&
              book.id.length <= 200,
          )
          .slice(0, 30);
      } catch (_) {
        return [];
      }
    }

    setCurrentJobID(jobID) {
      if (this.currentJobID !== jobID) {
        this.pollToken += 1;
        this.resetChapterDownloads();
        this.selectedWarningIDs.clear();
        this.warningFragments = [];
        this.expandedRevisionIDs.clear();
        this.nodes.warningsList.replaceChildren();
        this.nodes.warningsSection.hidden = true;
        if (
          this.currentRewrite &&
          this.currentRewrite.job_id === jobID
        ) {
          this.renderRewriteTask(this.currentRewrite);
        } else {
          this.nodes.rewriteProgressRegion.hidden = true;
        }
        this.updateRetrySelectedButton();
      }
      this.currentJobID = jobID;
      this.nodes.jobIDInput.value = jobID;
      this.writeStorage(STORAGE.lastJobID, jobID);
    }

    async copyCurrentJobID() {
      if (!this.currentJobID) {
        return;
      }
      try {
        await navigator.clipboard.writeText(this.currentJobID);
        this.notify("ID задачи скопирован.", "success");
      } catch (_) {
        this.nodes.jobIDInput.focus();
        this.nodes.jobIDInput.select();
        this.notify("ID выделен — скопируйте его вручную.");
      }
    }

    async withBusy(key, button, operation, notifyOnError = true) {
      if (this.busyOperations.has(key)) {
        return;
      }
      this.busyOperations.add(key);
      const wasDisabled = button.disabled;
      button.disabled = true;
      button.classList.add("is-busy");
      try {
        await operation();
      } catch (error) {
        if (notifyOnError) {
          this.notifyError(error, "Операция не выполнена");
        } else {
          throw error;
        }
      } finally {
        this.busyOperations.delete(key);
        button.disabled = wasDisabled;
        button.classList.remove("is-busy");
        this.renderSelectionSummary();
        this.updateRetrySelectedButton();
      }
    }

    notifyError(error, fallback) {
      const message =
        error && typeof error.message === "string" && error.message
          ? error.message
          : fallback;
      this.notify(`${fallback}: ${message}`, "error");
    }

    notify(message, kind = "info") {
      const toast = this.createNode(
        "div",
        `toast toast--${kind}`,
        String(message),
      );
      toast.setAttribute("role", kind === "error" ? "alert" : "status");
      this.nodes.toastRegion.append(toast);
      window.setTimeout(() => toast.remove(), 5200);
    }

    createNode(tagName, className = "", text = "") {
      const node = this.document.createElement(tagName);
      if (className) {
        node.className = className;
      }
      if (text !== "") {
        node.textContent = String(text);
      }
      return node;
    }

    byID(id) {
      const node = this.document.getElementById(id);
      if (!node) {
        throw new Error(`UI element #${id} is missing`);
      }
      return node;
    }

    describeFile(file) {
      const size = file.size < 1024 * 1024
        ? `${Math.max(1, Math.round(file.size / 1024))} КБ`
        : `${(file.size / (1024 * 1024)).toFixed(1)} МБ`;
      return `${file.name} · ${size}`;
    }

    formatDate(value) {
      const date = new Date(value);
      if (Number.isNaN(date.getTime())) {
        return "только что";
      }
      return new Intl.DateTimeFormat("ru-RU", {
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
      }).format(date);
    }

    formatLongDate(value) {
      const date = new Date(value);
      if (Number.isNaN(date.getTime())) {
        return "время не указано";
      }
      return new Intl.DateTimeFormat("ru-RU", {
        dateStyle: "medium",
        timeStyle: "short",
      }).format(date);
    }

    formatBytes(value) {
      const bytes = Number(value);
      if (!Number.isFinite(bytes) || bytes <= 0) {
        return "";
      }
      if (bytes >= 1024 * 1024 * 1024) {
        return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)} ГБ`;
      }
      return `${Math.max(1, Math.round(bytes / (1024 * 1024)))} МБ`;
    }

    chapterFLACFilename(chapter) {
      const provided = String(
        (chapter && chapter.audio_filename) || "",
      ).trim();
      const filename = provided
        ? provided.replaceAll("\\", "/").split("/").pop()
        : "";
      if (
        filename &&
        filename.length <= 240 &&
        filename.toLowerCase().endsWith(".flac")
      ) {
        return filename.replace(
          /[\u0000-\u001f<>:"/\\|?*]+/g,
          " ",
        );
      }

      const number = this.number(chapter && chapter.chapter_number);
      const rawTitle = String(
        (chapter && chapter.title) || `Глава ${number}`,
      );
      const title = rawTitle
        .replace(/[\u0000-\u001f<>:"/\\|?*]+/g, " ")
        .replace(/\s+/g, " ")
        .trim()
        .replace(/[. ]+$/g, "")
        .slice(0, 160);
      return (
        `character_${String(number).padStart(4, "0")}_` +
        `${title || `Глава ${number}`}.flac`
      );
    }

    shortID(value) {
      const normalized = String(value || "").trim();
      if (!normalized) {
        return "не указан";
      }
      return normalized.length > 14
        ? `${normalized.slice(0, 8)}…${normalized.slice(-4)}`
        : normalized;
    }

    number(value) {
      const parsed = Number(value);
      return Number.isFinite(parsed) && parsed >= 0 ? parsed : 0;
    }

    safeDOMID(value) {
      return String(value).replace(/[^A-Za-z0-9_-]/g, "_");
    }

    readStorage(key) {
      try {
        return window.localStorage.getItem(key);
      } catch (_) {
        return null;
      }
    }

    writeStorage(key, value) {
      try {
        window.localStorage.setItem(key, value);
      } catch (_) {
        // Private mode or a full quota must not break the application.
      }
    }

    removeStorage(key) {
      try {
        window.localStorage.removeItem(key);
      } catch (_) {
        // Private mode or disabled storage must not break the application.
      }
    }
  }

  window.addEventListener("DOMContentLoaded", () => {
    const app = new AudiobookApp(document);
    app.init();
  });
})();
