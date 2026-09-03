(() => {
  "use strict";

  const PAGE_SIZE = 20;
  const POLL_INTERVAL_MS = 5000;
  const REWRITE_POLL_INTERVAL_MS = 1800;
  const REWRITE_SELECTED_LABEL = "Переписать выбранные";
  const REWRITE_ALL_LABEL = "Переписать все warning этой главы";
  const REWRITE_STORAGE_KEY = "audiobook-ui.chapter-rewrite.v1";
  const REWRITE_TERMINAL_STATUSES = new Set([
    "completed",
    "completed_with_errors",
    "failed",
  ]);
  const STATUS_LABELS = Object.freeze({
    pending: "Ожидает",
    generating: "Озвучивается",
    ready: "Готов",
    warning: "Проверить",
    failed: "Ошибка",
  });
  const REWRITE_STATUS_LABELS = Object.freeze({
    queued: "LLM-задача ожидает запуска",
    running: "Локальная LLM переписывает фрагменты",
    completed: "LLM-правка завершена",
    completed_with_errors: "LLM-правка завершена с ошибками",
    failed: "LLM-правка не выполнена",
  });

  class APIError extends Error {
    constructor(status, code, message) {
      super(message || `HTTP ${status}`);
      this.name = "APIError";
      this.status = status;
      this.code = code || "HTTP_ERROR";
    }
  }

  class ChapterAPI {
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
      } catch (_) {
        throw new APIError(0, "NETWORK_ERROR", "Go API недоступен.");
      }
      if (!response.ok) {
        let problem = null;
        try {
          problem = await response.json();
        } catch (_) {
          // Reverse proxies can return a non-JSON error page.
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
      return response.json();
    }

    chapter(jobID, chapterNumber) {
      return this.request(
        `/v1/job/${encodeURIComponent(jobID)}/chapters/` +
          `${encodeURIComponent(chapterNumber)}`,
      );
    }

    edit(fragmentID, text) {
      return this.request(`/v1/fragment/${encodeURIComponent(fragmentID)}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ new_text: text }),
      });
    }

    retry(jobID, fragmentID) {
      return this.request(
        `/v1/job/${encodeURIComponent(jobID)}/retry/warnings`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ fragment_ids: [fragmentID] }),
        },
      );
    }

    approve(fragmentID) {
      return this.request(
        `/v1/fragment/${encodeURIComponent(fragmentID)}/approve`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ reason: "Проверено на странице главы" }),
        },
      );
    }

    rewriteModels() {
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

    rewriteTask(rewriteID) {
      return this.request(`/v1/rewrite/${encodeURIComponent(rewriteID)}`);
    }
  }

  class ChapterPage {
    constructor(documentRoot) {
      this.document = documentRoot;
      this.api = new ChapterAPI();
      this.jobID = "";
      this.chapterNumber = 0;
      this.snapshot = null;
      this.page = 0;
      this.busy = new Set();
      this.timer = null;
      this.toastTimer = null;
      this.selectedRewriteIDs = new Set();
      this.rewriteModelsResponse = null;
      this.activeRewrite = null;
      this.rewritePollToken = 0;
      this.snapshotRenderSignature = "";
      this.fragmentRenderSignature = "";
      this.nodes = {
        title: this.required("chapter-title"),
        subtitle: this.required("chapter-subtitle"),
        status: this.required("chapter-status"),
        summary: this.required("summary-message"),
        download: this.required("download-link"),
        refresh: this.required("refresh-button"),
        total: this.required("metric-total"),
        voiced: this.required("metric-voiced"),
        ready: this.required("metric-ready"),
        warning: this.required("metric-warning"),
        pending: this.required("metric-pending"),
        failed: this.required("metric-failed"),
        progress: this.required("chapter-progress"),
        refreshState: this.required("refresh-state"),
        filter: this.required("status-filter"),
        search: this.required("fragment-search"),
        list: this.required("fragment-list"),
        empty: this.required("empty-state"),
        previous: this.required("page-prev"),
        next: this.required("page-next"),
        pageLabel: this.required("page-label"),
        toast: this.required("toast"),
        rewritePanel: this.required("rewrite-panel"),
        rewriteModelRefresh: this.required("rewrite-model-refresh"),
        rewriteModelSelect: this.required("rewrite-model-select"),
        rewritePrompt: this.required("rewrite-prompt"),
        rewriteTemperature: this.required("rewrite-temperature"),
        rewriteTemperatureHint: this.required("rewrite-temperature-hint"),
        rewriteTopK: this.required("rewrite-top-k"),
        rewriteTopKHint: this.required("rewrite-top-k-hint"),
        rewriteTopP: this.required("rewrite-top-p"),
        rewriteTopPHint: this.required("rewrite-top-p-hint"),
        rewriteMinP: this.required("rewrite-min-p"),
        rewriteMinPHint: this.required("rewrite-min-p-hint"),
        rewriteRepeatPenalty: this.required("rewrite-repeat-penalty"),
        rewriteRepeatPenaltyHint: this.required("rewrite-repeat-penalty-hint"),
        rewriteMaxTokens: this.required("rewrite-max-tokens"),
        rewriteMaxTokensHint: this.required("rewrite-max-tokens-hint"),
        rewriteModelStatus: this.required("rewrite-model-status"),
        rewriteSelectionSummary: this.required("rewrite-selection-summary"),
        rewriteSelectedButton: this.required("rewrite-selected-button"),
        rewriteAllButton: this.required("rewrite-all-button"),
        rewriteProgressRegion: this.required("rewrite-progress-region"),
        rewriteProgressLabel: this.required("rewrite-progress-label"),
        rewriteProgressPercent: this.required("rewrite-progress-percent"),
        rewriteProgressBar: this.required("rewrite-progress-bar"),
        rewriteProgressDetails: this.required("rewrite-progress-details"),
        rewriteFailures: this.required("rewrite-failures"),
      };
    }

    required(id) {
      const node = this.document.getElementById(id);
      if (!node) {
        throw new Error(`UI element #${id} is missing`);
      }
      return node;
    }

    init() {
      const match = window.location.pathname.match(
        /^\/jobs\/([^/]+)\/chapters\/(\d+)$/,
      );
      if (!match) {
        this.failPage("Некорректный адрес главы.");
        return;
      }
      this.jobID = decodeURIComponent(match[1]);
      this.chapterNumber = Number(match[2]);

      this.nodes.refresh.addEventListener("click", () => this.refresh(true));
      this.nodes.filter.addEventListener("change", () => {
        this.page = 0;
        this.renderFragments();
      });
      this.nodes.search.addEventListener("input", () => {
        this.page = 0;
        this.renderFragments();
      });
      this.nodes.previous.addEventListener("click", () => {
        if (this.page > 0) {
          this.page -= 1;
          this.renderFragments();
        }
      });
      this.nodes.next.addEventListener("click", () => {
        const pages = this.pageCount();
        if (this.page + 1 < pages) {
          this.page += 1;
          this.renderFragments();
        }
      });
      this.nodes.download.addEventListener("click", (event) => {
        if (this.nodes.download.getAttribute("aria-disabled") === "true") {
          event.preventDefault();
          this.notify(
            "Скачивание появится, когда у каждого фрагмента будет аудио.",
            "error",
          );
        }
      });
      this.nodes.rewriteModelRefresh.addEventListener(
        "click",
        () => this.loadRewriteModels(true),
      );
      this.nodes.rewriteModelSelect.addEventListener(
        "change",
        () => this.renderSelectedRewriteModelStatus(),
      );
      this.nodes.rewriteSelectedButton.textContent = REWRITE_SELECTED_LABEL;
      this.nodes.rewriteAllButton.textContent = REWRITE_ALL_LABEL;
      this.nodes.rewriteSelectedButton.addEventListener(
        "click",
        () => this.startRewrite(false),
      );
      this.nodes.rewriteAllButton.addEventListener(
        "click",
        () => this.startRewrite(true),
      );

      this.resumeStoredRewrite();
      this.refresh(true);
      this.schedule();
    }

    isReviewBusy() {
      const active = this.document.activeElement;
      if (
        active &&
        (this.nodes.list.contains(active) || this.nodes.rewritePanel.contains(active)) &&
        ["TEXTAREA", "INPUT", "SELECT"].includes(active.tagName)
      ) {
        return true;
      }
      return Array.from(this.nodes.list.querySelectorAll("audio")).some(
        (audio) => !audio.paused,
      );
    }

    schedule() {
      window.clearTimeout(this.timer);
      this.timer = window.setTimeout(async () => {
        if (
          !this.document.hidden &&
          !this.isReviewBusy() &&
          this.busy.size === 0
        ) {
          await this.refresh(false);
        } else if (this.isReviewBusy()) {
          this.nodes.refreshState.textContent =
            "Автообновление приостановлено на время правки, настройки LLM или прослушивания.";
        }
        this.schedule();
      }, POLL_INTERVAL_MS);
    }

    async refresh(announce) {
      if (this.busy.has("refresh")) {
        return;
      }
      const showBusy = announce || !this.snapshot;
      this.busy.add("refresh");
      if (showBusy) {
        this.nodes.refresh.disabled = true;
        this.nodes.refresh.classList.add("is-busy");
      }
      try {
        const snapshot = await this.api.chapter(this.jobID, this.chapterNumber);
        const signature = JSON.stringify(snapshot);
        const changed = signature !== this.snapshotRenderSignature;
        this.snapshot = snapshot;
        if (changed) {
          this.snapshotRenderSignature = signature;
          this.pruneRewriteSelection();
          this.render();
        }
        this.nodes.refreshState.textContent =
          `Обновлено ${new Intl.DateTimeFormat("ru-RU", {
            hour: "2-digit",
            minute: "2-digit",
            second: "2-digit",
          }).format(new Date())}`;
        if (announce) {
          this.notify("Данные главы обновлены.");
        }
        if (this.warningFragments().length > 0 && !this.rewriteModelsResponse) {
          this.loadRewriteModels(false);
        }
      } catch (error) {
        this.failPage(error.message || "Не удалось загрузить главу.");
        if (announce) {
          this.notify(error.message, "error");
        }
      } finally {
        this.busy.delete("refresh");
        if (showBusy) {
          this.nodes.refresh.disabled = false;
          this.nodes.refresh.classList.remove("is-busy");
        }
      }
    }

    render() {
      const chapter = this.snapshot && this.snapshot.chapter;
      if (!chapter) {
        return;
      }
      const title = chapter.title || `Глава ${chapter.chapter_number}`;
      this.document.title = `${title} — Голос Книги`;
      this.nodes.title.textContent = title;
      this.nodes.subtitle.textContent =
        `Задача ${this.snapshot.job_id} · глава ${chapter.chapter_number}`;
      this.nodes.total.textContent = String(this.number(chapter.fragments_count));
      this.nodes.voiced.textContent = String(this.number(chapter.fragments_voiced));
      this.nodes.ready.textContent = String(this.number(chapter.fragments_ready));
      this.nodes.warning.textContent = String(this.number(chapter.fragments_warning));
      this.nodes.pending.textContent = String(this.number(chapter.fragments_pending));
      this.nodes.failed.textContent = String(this.number(chapter.fragments_failed));

      const total = this.number(chapter.fragments_count);
      const voiced = this.number(chapter.fragments_voiced);
      const percentage = total > 0 ? Math.round((voiced / total) * 100) : 0;
      this.nodes.progress.value = percentage;
      this.nodes.progress.textContent = `${percentage}%`;

      const review = this.number(chapter.review_required);
      this.nodes.status.textContent = chapter.audio_complete
        ? review > 0
          ? "Озвучено · есть замечания"
          : "Готово"
        : "Озвучивание продолжается";
      this.nodes.summary.textContent = chapter.audio_complete
        ? review > 0
          ? `Все ${total} фрагментов имеют аудио. ${review} можно проверить, но скачивание уже доступно.`
          : `Все ${total} фрагментов озвучены без замечаний.`
        : `Озвучено ${voiced} из ${total}. Страница показывает только одну главу и не перегружает основной интерфейс.`;

      if (chapter.audio_complete && chapter.audio_url) {
        this.nodes.download.href = chapter.audio_url;
        this.nodes.download.download =
          chapter.audio_filename || `chapter-${chapter.chapter_number}.flac`;
        this.nodes.download.classList.remove("is-disabled");
        this.nodes.download.setAttribute("aria-disabled", "false");
      } else {
        this.nodes.download.removeAttribute("href");
        this.nodes.download.removeAttribute("download");
        this.nodes.download.classList.add("is-disabled");
        this.nodes.download.setAttribute("aria-disabled", "true");
      }
      this.renderFragments();
      this.updateRewriteControls();
    }

    allFragments() {
      return this.snapshot && Array.isArray(this.snapshot.fragments)
        ? this.snapshot.fragments
        : [];
    }

    warningFragments() {
      return this.allFragments().filter(
        (fragment) => fragment.status === "warning",
      );
    }

    pruneRewriteSelection() {
      const allowed = new Set(this.warningFragments().map((fragment) => fragment.id));
      for (const id of this.selectedRewriteIDs) {
        if (!allowed.has(id)) {
          this.selectedRewriteIDs.delete(id);
        }
      }
    }

    filteredFragments() {
      const filter = this.nodes.filter.value;
      const query = this.nodes.search.value.trim().toLocaleLowerCase("ru-RU");
      return this.allFragments()
        .filter((fragment) => {
          if (
            filter === "issues" &&
            fragment.status !== "warning" &&
            fragment.status !== "failed"
          ) {
            return false;
          }
          if (filter === "ready" && fragment.status !== "ready") {
            return false;
          }
          if (
            filter === "in_progress" &&
            fragment.status !== "pending" &&
            fragment.status !== "generating"
          ) {
            return false;
          }
          if (query) {
            const haystack = [
              fragment.text,
              fragment.stt_text,
              fragment.warning_code,
              fragment.error,
              String(fragment.ordinal || ""),
            ]
              .join(" ")
              .toLocaleLowerCase("ru-RU");
            if (!haystack.includes(query)) {
              return false;
            }
          }
          return true;
        })
        .sort(
          (left, right) =>
            this.number(left.ordinal) - this.number(right.ordinal),
        );
    }

    pageCount() {
      return Math.max(1, Math.ceil(this.filteredFragments().length / PAGE_SIZE));
    }

    renderFragments() {
      if (!this.snapshot) {
        return;
      }
      const fragments = this.filteredFragments();
      const pages = Math.max(1, Math.ceil(fragments.length / PAGE_SIZE));
      if (this.page >= pages) {
        this.page = pages - 1;
      }
      const visible = fragments.slice(
        this.page * PAGE_SIZE,
        (this.page + 1) * PAGE_SIZE,
      );
      const signature = JSON.stringify({
        filter: this.nodes.filter.value,
        query: this.nodes.search.value,
        page: this.page,
        selected: Array.from(this.selectedRewriteIDs).sort(),
        visible,
      });
      if (signature === this.fragmentRenderSignature) {
        return;
      }
      this.fragmentRenderSignature = signature;
      this.nodes.list.replaceChildren();
      for (const fragment of visible) {
        this.nodes.list.append(this.createCard(fragment));
      }
      this.nodes.empty.hidden = fragments.length !== 0;
      this.nodes.previous.disabled = this.page === 0;
      this.nodes.next.disabled = this.page + 1 >= pages;
      this.nodes.pageLabel.textContent =
        `Страница ${this.page + 1} из ${pages} · ${fragments.length} фрагм.`;
    }

    createCard(fragment) {
      const card = this.create("article", "fragment-card");
      card.dataset.status = fragment.status || "pending";

      const header = this.create("header");
      const identity = this.create("div");
      identity.append(
        this.create(
          "strong",
          "",
          `Фрагмент ${this.number(fragment.ordinal)}`,
        ),
        this.create(
          "small",
          "",
          `Попыток: ${this.number(fragment.attempt)} · ` +
            `Whisper: ${this.scoreLabel(fragment.transcript_score)}`,
        ),
      );
      const badge = this.create(
        "span",
        "fragment-status",
        STATUS_LABELS[fragment.status] || fragment.status || "Ожидает",
      );
      header.append(identity, badge);

      const body = this.create("div", "fragment-body");
      const editor = this.create("label", "fragment-editor");
      editor.append(this.create("span", "", "Текст для озвучивания"));
      const textarea = this.create("textarea");
      textarea.value = fragment.text || "";
      textarea.maxLength = 20000;
      textarea.readOnly = !fragment.editable;
      textarea.setAttribute(
        "aria-label",
        `Текст фрагмента ${this.number(fragment.ordinal)}`,
      );
      editor.append(textarea);

      const recognition = this.create("div", "recognition");
      recognition.append(this.create("span", "", "Распознано Whisper"));
      recognition.append(
        this.create(
          "pre",
          "",
          fragment.stt_text || "Расшифровка ещё не получена.",
        ),
      );
      recognition.append(
        this.create(
          "span",
          "score",
          `Сходство: ${this.scoreLabel(fragment.transcript_score)}`,
        ),
      );
      if (fragment.audio_available) {
        const audio = this.create("audio");
        audio.controls = true;
        audio.preload = "none";
        audio.src =
          `/v1/fragment/${encodeURIComponent(fragment.id)}/audio.wav`;
        recognition.append(audio);
      }
      if (fragment.error) {
        recognition.append(
          this.create("p", "fragment-error", fragment.error),
        );
      }
      body.append(editor, recognition);

      const actions = this.create("div", "fragment-actions");
      if (fragment.status === "warning") {
        const selection = this.create("label", "rewrite-fragment-selection");
        const checkbox = this.create("input");
        checkbox.type = "checkbox";
        checkbox.checked = this.selectedRewriteIDs.has(fragment.id);
        checkbox.setAttribute(
          "aria-label",
          `Выбрать фрагмент ${this.number(fragment.ordinal)} для LLM`,
        );
        checkbox.addEventListener("change", () => {
          if (checkbox.checked) {
            this.selectedRewriteIDs.add(fragment.id);
          } else {
            this.selectedRewriteIDs.delete(fragment.id);
          }
          this.updateRewriteControls();
        });
        selection.append(checkbox, this.create("span", "", "Выбрать для LLM"));
        actions.append(selection);
      }
      if (fragment.editable) {
        const save = this.button("Сохранить и переозвучить");
        save.addEventListener(
          "click",
          () => this.edit(fragment, textarea, save),
        );
        actions.append(save);
      }
      if (fragment.status === "warning" || fragment.status === "failed") {
        const retry = this.button("Повторить только этот фрагмент");
        retry.addEventListener(
          "click",
          () => this.retry(fragment, retry),
        );
        actions.append(retry);
      }
      if (fragment.status === "warning" && fragment.audio_available) {
        const approve = this.button("Принять текущее аудио");
        approve.addEventListener(
          "click",
          () => this.approve(fragment, approve),
        );
        actions.append(approve);
      }
      card.append(header, body, actions);
      return card;
    }

    async edit(fragment, textarea, button) {
      const text = textarea.value.trim();
      if (!text) {
        this.notify("Текст не может быть пустым.", "error");
        textarea.focus();
        return;
      }
      await this.runAction(`edit-${fragment.id}`, button, async () => {
        await this.api.edit(fragment.id, text);
        this.selectedRewriteIDs.delete(fragment.id);
        this.notify("Правка сохранена; фрагмент поставлен в очередь.");
      });
    }

    async retry(fragment, button) {
      await this.runAction(`retry-${fragment.id}`, button, async () => {
        await this.api.retry(this.jobID, fragment.id);
        this.selectedRewriteIDs.delete(fragment.id);
        this.notify(
          "Один фрагмент поставлен на повтор. Будет сохранена лучшая попытка по Whisper.",
        );
      });
    }

    async approve(fragment, button) {
      await this.runAction(`approve-${fragment.id}`, button, async () => {
        await this.api.approve(fragment.id);
        this.selectedRewriteIDs.delete(fragment.id);
        this.notify("Текущее аудио принято без повторной генерации.");
      });
    }

    async runAction(key, button, operation) {
      if (this.busy.has(key)) {
        return;
      }
      this.busy.add(key);
      button.disabled = true;
      button.classList.add("is-busy");
      try {
        await operation();
        await this.refresh(false);
      } catch (error) {
        this.notify(error.message || "Операция не выполнена.", "error");
      } finally {
        this.busy.delete(key);
        button.classList.remove("is-busy");
        this.updateRewriteControls();
      }
    }

    async loadRewriteModels(force) {
      if (this.busy.has("rewrite-models")) {
        return;
      }
      if (this.rewriteModelsResponse && !force) {
        this.updateRewriteControls();
        return;
      }
      this.busy.add("rewrite-models");
      this.nodes.rewriteModelRefresh.disabled = true;
      this.nodes.rewriteModelRefresh.classList.add("is-busy");
      this.nodes.rewriteModelStatus.textContent =
        "Получаем список разрешённых локальных моделей…";
      try {
        const response = await this.api.rewriteModels();
        const models = Array.isArray(response && response.models)
          ? response.models
          : [];
        this.rewriteModelsResponse = { ...response, models };
        this.nodes.rewriteModelSelect.replaceChildren();
        if (models.length === 0) {
          this.nodes.rewriteModelSelect.append(
            new Option("Нет доступных моделей", ""),
          );
          this.nodes.rewriteModelStatus.textContent =
            "Go API не вернул ни одной разрешённой LLM.";
          return;
        }
        for (const model of models) {
          const attributes = [];
          if (model.recommended) {
            attributes.push("рекомендуется");
          }
          if (model.loaded) {
            attributes.push("загружена");
          } else if (model.available) {
            attributes.push("локально");
          } else {
            attributes.push("скачается при запуске");
          }
          const title = model.display_name || model.id;
          this.nodes.rewriteModelSelect.append(
            new Option(
              `${title}${attributes.length ? ` · ${attributes.join(" · ")}` : ""}`,
              model.id,
            ),
          );
        }
        const preferred = models.some(
          (model) => model.id === response.default_model_id,
        )
          ? response.default_model_id
          : (models.find((model) => model.recommended) || models[0]).id;
        this.nodes.rewriteModelSelect.value = preferred;

        if (!this.nodes.rewritePrompt.value.trim()) {
          this.nodes.rewritePrompt.value = response.default_prompt || "";
        }
        const maxPromptLength = this.number(
          response.limits && response.limits.max_prompt_chars,
        );
        if (maxPromptLength > 0) {
          this.nodes.rewritePrompt.maxLength = maxPromptLength;
        }
        const settings = response.settings || {};
        this.configureRewriteNumber(
          this.nodes.rewriteTemperature,
          this.nodes.rewriteTemperatureHint,
          settings.temperature,
          false,
        );
        this.configureRewriteNumber(
          this.nodes.rewriteTopK,
          this.nodes.rewriteTopKHint,
          settings.top_k,
          true,
        );
        this.configureRewriteNumber(
          this.nodes.rewriteTopP,
          this.nodes.rewriteTopPHint,
          settings.top_p,
          false,
        );
        this.configureRewriteNumber(
          this.nodes.rewriteMinP,
          this.nodes.rewriteMinPHint,
          settings.min_p,
          false,
        );
        this.configureRewriteNumber(
          this.nodes.rewriteRepeatPenalty,
          this.nodes.rewriteRepeatPenaltyHint,
          settings.repeat_penalty,
          false,
        );
        this.configureRewriteNumber(
          this.nodes.rewriteMaxTokens,
          this.nodes.rewriteMaxTokensHint,
          settings.max_tokens,
          true,
        );
        this.renderSelectedRewriteModelStatus();
      } catch (error) {
        this.rewriteModelsResponse = null;
        this.nodes.rewriteModelStatus.textContent =
          "Локальная LLM недоступна. Проверьте rewriter worker и повторите запрос.";
        this.notify(error.message || "Не удалось получить модели.", "error");
      } finally {
        this.busy.delete("rewrite-models");
        this.nodes.rewriteModelRefresh.classList.remove("is-busy");
        this.updateRewriteControls();
      }
    }

    configureRewriteNumber(input, hint, setting, integer) {
      const minimum = Number(setting && setting.minimum);
      const maximum = Number(setting && setting.maximum);
      const defaultValue = Number(setting && setting.default);
      const hasRange =
        Number.isFinite(minimum) &&
        Number.isFinite(maximum) &&
        minimum <= maximum;
      if (hasRange) {
        input.min = String(minimum);
        input.max = String(maximum);
        input.step = integer ? "1" : "0.01";
      } else {
        input.removeAttribute("min");
        input.removeAttribute("max");
        input.step = integer ? "1" : "any";
      }
      if (!input.value && Number.isFinite(defaultValue)) {
        input.value = String(defaultValue);
      }
      hint.textContent = hasRange
        ? `Допустимо: ${minimum}–${maximum}; по умолчанию ${
            Number.isFinite(defaultValue) ? defaultValue : minimum
          }.`
        : "Диапазон не получен от API.";
    }

    renderSelectedRewriteModelStatus() {
      const response = this.rewriteModelsResponse;
      const model = response && Array.isArray(response.models)
        ? response.models.find(
            (candidate) =>
              candidate.id === this.nodes.rewriteModelSelect.value,
          )
        : null;
      if (!model) {
        this.nodes.rewriteModelStatus.textContent =
          "Выберите модель для локальной LLM-правки.";
        return;
      }
      const name = model.display_name || model.id;
      if (model.loaded) {
        this.nodes.rewriteModelStatus.textContent =
          `${name} уже загружена и готова на ${model.device || "CPU"}.`;
      } else if (model.available) {
        this.nodes.rewriteModelStatus.textContent =
          `${name} находится на сервере; первый запуск может занять несколько минут.`;
      } else {
        this.nodes.rewriteModelStatus.textContent =
          `${name} будет скачана при первом запуске. Задача продолжится на сервере после закрытия страницы.`;
      }
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

    async startRewrite(allWarnings) {
      if (this.isRewriteActive()) {
        this.notify("LLM-правка уже выполняется.", "error");
        return;
      }
      if (!this.rewriteModelsResponse) {
        this.notify("Сначала дождитесь списка локальных моделей.", "error");
        this.loadRewriteModels(false);
        return;
      }
      const warningIDs = this.warningFragments().map((fragment) => fragment.id);
      const allowed = new Set(warningIDs);
      const selected = allWarnings
        ? warningIDs
        : Array.from(this.selectedRewriteIDs).filter((id) => allowed.has(id));
      if (selected.length === 0) {
        this.notify(
          allWarnings
            ? "В этой главе нет warning-фрагментов для LLM."
            : "Отметьте хотя бы один warning-фрагмент.",
          "error",
        );
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
      const settings = this.rewriteModelsResponse.settings || {};
      const temperature = this.readRewriteNumber(
        this.nodes.rewriteTemperature,
        "Temperature",
        settings.temperature,
        false,
      );
      const topK = this.readRewriteNumber(
        this.nodes.rewriteTopK,
        "Top K",
        settings.top_k,
        true,
      );
      const topP = this.readRewriteNumber(
        this.nodes.rewriteTopP,
        "Top P",
        settings.top_p,
        false,
      );
      const minP = this.readRewriteNumber(
        this.nodes.rewriteMinP,
        "Min P",
        settings.min_p,
        false,
      );
      const repeatPenalty = this.readRewriteNumber(
        this.nodes.rewriteRepeatPenalty,
        "Repeat penalty",
        settings.repeat_penalty,
        false,
      );
      const maxTokens = this.readRewriteNumber(
        this.nodes.rewriteMaxTokens,
        "Максимум токенов",
        settings.max_tokens,
        true,
      );
      if (
        [temperature, topK, topP, minP, repeatPenalty, maxTokens].some(
          (value) => value === null,
        )
      ) {
        return;
      }

      const button = allWarnings
        ? this.nodes.rewriteAllButton
        : this.nodes.rewriteSelectedButton;
      const key = "rewrite-start";
      if (this.busy.has(key)) {
        return;
      }
      this.busy.add(key);
      button.disabled = true;
      button.classList.add("is-busy");
      try {
        const response = await this.api.rewriteWarnings(this.jobID, {
          fragment_ids: selected,
          model_id: modelID,
          prompt,
          temperature,
          top_k: topK,
          top_p: topP,
          min_p: minP,
          repeat_penalty: repeatPenalty,
          max_tokens: maxTokens,
        });
        const rewriteID = response && (response.id || response.rewrite_id);
        if (!rewriteID) {
          throw new Error("API не вернул ID LLM-задачи.");
        }
        this.activeRewrite = {
          ...response,
          id: rewriteID,
          job_id: (response && response.job_id) || this.jobID,
          chapter_number: this.chapterNumber,
          status: (response && response.status) || "queued",
        };
        this.persistRewrite(this.activeRewrite);
        this.renderRewriteProgress(this.activeRewrite);
        this.updateRewriteControls();
        this.pollRewrite(rewriteID);
        this.notify(
          `LLM-задача для ${selected.length} фрагм. поставлена в очередь.`,
        );
      } catch (error) {
        this.notify(error.message || "LLM-задача не создана.", "error");
      } finally {
        this.busy.delete(key);
        button.classList.remove("is-busy");
        this.updateRewriteControls();
      }
    }

    isRewriteActive() {
      return Boolean(
        this.activeRewrite &&
          !REWRITE_TERMINAL_STATUSES.has(this.activeRewrite.status),
      );
    }

    persistRewrite(task) {
      try {
        window.localStorage.setItem(
          REWRITE_STORAGE_KEY,
          JSON.stringify({
            id: task.id,
            job_id: task.job_id || this.jobID,
            chapter_number: this.chapterNumber,
            status: task.status || "queued",
          }),
        );
      } catch (_) {
        // The server task remains valid even if browser storage is unavailable.
      }
    }

    clearStoredRewrite(rewriteID) {
      try {
        const raw = window.localStorage.getItem(REWRITE_STORAGE_KEY);
        if (!raw) {
          return;
        }
        const stored = JSON.parse(raw);
        if (!stored || !stored.id || stored.id === rewriteID) {
          window.localStorage.removeItem(REWRITE_STORAGE_KEY);
        }
      } catch (_) {
        try {
          window.localStorage.removeItem(REWRITE_STORAGE_KEY);
        } catch (_) {
          // Ignore unavailable storage.
        }
      }
    }

    resumeStoredRewrite() {
      let stored = null;
      try {
        const raw = window.localStorage.getItem(REWRITE_STORAGE_KEY);
        stored = raw ? JSON.parse(raw) : null;
      } catch (_) {
        return;
      }
      if (
        !stored ||
        typeof stored.id !== "string" ||
        !stored.id ||
        stored.job_id !== this.jobID ||
        Number(stored.chapter_number) !== this.chapterNumber ||
        REWRITE_TERMINAL_STATUSES.has(stored.status)
      ) {
        return;
      }
      this.activeRewrite = stored;
      this.nodes.rewriteProgressRegion.hidden = false;
      this.nodes.rewriteProgressLabel.textContent =
        "Восстанавливаем состояние LLM-задачи…";
      this.nodes.rewriteProgressDetails.textContent =
        "Получаем актуальное состояние с сервера.";
      this.updateRewriteControls();
      this.pollRewrite(stored.id);
    }

    pollRewrite(rewriteID) {
      this.rewritePollToken += 1;
      const token = this.rewritePollToken;
      let consecutiveErrors = 0;

      const poll = async () => {
        if (token !== this.rewritePollToken) {
          return;
        }
        if (this.document.hidden) {
          window.setTimeout(poll, REWRITE_POLL_INTERVAL_MS);
          return;
        }
        try {
          const task = await this.api.rewriteTask(rewriteID);
          if (token !== this.rewritePollToken) {
            return;
          }
          this.activeRewrite = {
            ...task,
            chapter_number: this.chapterNumber,
          };
          this.persistRewrite(this.activeRewrite);
          this.renderRewriteProgress(this.activeRewrite);
          this.updateRewriteControls();
          consecutiveErrors = 0;

          if (REWRITE_TERMINAL_STATUSES.has(task.status)) {
            this.clearStoredRewrite(rewriteID);
            this.activeRewrite = null;
            this.selectedRewriteIDs.clear();
            this.updateRewriteControls();
            if (task.status === "completed") {
              this.notify(
                "LLM-правка завершена. Проверьте новый текст и отправьте нужные фрагменты на переозвучивание.",
              );
            } else if (task.status === "completed_with_errors") {
              this.notify(
                "LLM-правка завершена, но часть фрагментов обработать не удалось.",
                "error",
              );
            } else {
              this.notify(task.error || "LLM-правка не выполнена.", "error");
            }
            await this.refresh(false);
            return;
          }
        } catch (error) {
          if (error instanceof APIError && error.status === 404) {
            this.clearStoredRewrite(rewriteID);
            this.activeRewrite = null;
            this.updateRewriteControls();
            this.notify("Сохранённая LLM-задача больше не существует.", "error");
            return;
          }
          consecutiveErrors += 1;
          this.nodes.rewriteProgressDetails.textContent =
            "Связь с LLM-задачей временно потеряна; повторяем запрос автоматически.";
          if (consecutiveErrors === 1) {
            this.notify(error.message || "Не удалось обновить LLM-прогресс.", "error");
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

    renderRewriteProgress(task) {
      const total = this.number(task.fragments_count);
      const completed = this.number(task.fragments_completed);
      const failed = this.number(task.fragments_failed);
      const pending = Number.isFinite(Number(task.fragments_pending))
        ? this.number(task.fragments_pending)
        : Math.max(0, total - completed - failed);
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
        "Обновляем LLM-задачу";
      this.nodes.rewriteProgressPercent.textContent = `${percentage}%`;
      this.nodes.rewriteProgressBar.value = percentage;
      this.nodes.rewriteProgressBar.textContent = `${percentage}%`;
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
          : `Фрагмент ${fragment.fragment_id || "без ID"}`;
        this.nodes.rewriteFailures.append(
          this.create(
            "li",
            "",
            `${identity}: ${fragment.error || "причина не указана"}`,
          ),
        );
      }
      if (task.error && failedFragments.length === 0) {
        this.nodes.rewriteFailures.append(
          this.create("li", "", task.error),
        );
      }
      this.nodes.rewriteFailures.hidden =
        this.nodes.rewriteFailures.childElementCount === 0;
    }

    updateRewriteControls() {
      const warnings = this.warningFragments();
      const warningIDs = new Set(warnings.map((fragment) => fragment.id));
      let selected = 0;
      for (const id of this.selectedRewriteIDs) {
        if (warningIDs.has(id)) {
          selected += 1;
        }
      }
      const active = this.isRewriteActive();
      const models = this.rewriteModelsResponse &&
        Array.isArray(this.rewriteModelsResponse.models)
        ? this.rewriteModelsResponse.models
        : [];
      const modelReady = models.length > 0;
      this.nodes.rewriteSelectionSummary.textContent =
        `Выбрано: ${selected} · warning в главе: ${warnings.length}`;
      this.nodes.rewriteSelectedButton.disabled =
        active || !modelReady || selected === 0;
      this.nodes.rewriteAllButton.disabled =
        active || !modelReady || warnings.length === 0;
      this.nodes.rewriteModelRefresh.disabled =
        active || this.busy.has("rewrite-models");

      for (const node of [
        this.nodes.rewriteModelSelect,
        this.nodes.rewritePrompt,
        this.nodes.rewriteTemperature,
        this.nodes.rewriteTopK,
        this.nodes.rewriteTopP,
        this.nodes.rewriteMinP,
        this.nodes.rewriteRepeatPenalty,
        this.nodes.rewriteMaxTokens,
      ]) {
        node.disabled = active || !modelReady;
      }
      if (warnings.length === 0 && !active) {
        this.nodes.rewriteModelStatus.textContent =
          "В этой главе сейчас нет warning-фрагментов для LLM-правки.";
      } else if (modelReady && !active) {
        this.renderSelectedRewriteModelStatus();
      }
    }

    scoreLabel(value) {
      const score = Number(value);
      if (!Number.isFinite(score)) {
        return "нет оценки";
      }
      const percentage = Math.max(0, Math.min(1, score)) * 100;
      return `${percentage.toFixed(1).replace(".", ",")}%`;
    }

    number(value) {
      const parsed = Number(value);
      return Number.isFinite(parsed) ? parsed : 0;
    }

    button(label) {
      const button = this.create("button", "button button--quiet", label);
      button.type = "button";
      return button;
    }

    create(tag, className = "", text = "") {
      const node = this.document.createElement(tag);
      if (className) {
        node.className = className;
      }
      if (text) {
        node.textContent = text;
      }
      return node;
    }

    notify(message, kind = "success") {
      window.clearTimeout(this.toastTimer);
      this.nodes.toast.textContent = message || "Готово";
      this.nodes.toast.dataset.kind = kind;
      this.nodes.toast.hidden = false;
      this.toastTimer = window.setTimeout(() => {
        this.nodes.toast.hidden = true;
      }, 4500);
    }

    failPage(message) {
      this.nodes.status.textContent = "Ошибка";
      this.nodes.summary.textContent = message;
      this.nodes.refreshState.textContent =
        "Проверьте адрес или вернитесь к обзору задачи.";
    }
  }

  document.addEventListener("DOMContentLoaded", () => {
    try {
      new ChapterPage(document).init();
    } catch (error) {
      console.error("chapter UI initialization failed", error);
    }
  });
})();
