(() => {
  "use strict";

  const POLL_INTERVAL_MS = 2500;
  const PAGE_SIZE = 60;
  const STORAGE_KEYS = [
    "audiobook-ui.last-job-id.v1",
    "audiobook-ui.last-job-id.v2",
  ];
  const STATUS_LABELS = Object.freeze({
    pending: "Ожидает",
    generating: "Озвучивается",
    ready: "Готов",
    warning: "Проверить",
    failed: "Ошибка",
  });
  const ACTIONABLE_STATUSES = new Set(["ready", "warning", "failed"]);

  class APIError extends Error {
    constructor(status, code, message) {
      super(message || `HTTP ${status}`);
      this.name = "APIError";
      this.status = status;
      this.code = code || "HTTP_ERROR";
    }
  }

  class FragmentAPI {
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
          // Proxies can return non-JSON errors.
        }
        throw new APIError(
          response.status,
          problem && problem.code,
          problem && problem.error
            ? problem.error
            : `Запрос завершился с кодом ${response.status}`,
        );
      }
      return response.status === 204 ? null : response.json();
    }

    catalog(jobID) {
      return this.request(`/v1/job/${encodeURIComponent(jobID)}/chapters`);
    }

    edit(fragmentID, newText) {
      return this.request(`/v1/fragment/${encodeURIComponent(fragmentID)}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ new_text: newText }),
      });
    }
  }

  class FragmentStudio {
    constructor(documentRoot) {
      this.document = documentRoot;
      this.api = new FragmentAPI();
      this.activeJobID = "";
      this.snapshot = null;
      this.snapshotSignature = "";
      this.visibleLimit = PAGE_SIZE;
      this.loading = false;
      this.pendingRefresh = false;
      this.timer = null;
      this.nodes = {
        section: this.required("fragment-studio"),
        summary: this.required("fragment-studio-summary"),
        refreshed: this.required("fragment-studio-refreshed"),
        progress: this.required("fragment-studio-progress"),
        progressLabel: this.required("fragment-studio-progress-label"),
        total: this.required("fragment-count-total"),
        pending: this.required("fragment-count-pending"),
        ready: this.required("fragment-count-ready"),
        warning: this.required("fragment-count-warning"),
        failed: this.required("fragment-count-failed"),
        filter: this.required("fragment-filter"),
        search: this.required("fragment-search"),
        refresh: this.required("fragment-refresh-button"),
        list: this.required("fragment-studio-list"),
        more: this.required("fragment-load-more"),
        empty: this.required("fragment-studio-empty"),
      };
    }

    init() {
      this.nodes.filter.addEventListener("change", () => {
        this.visibleLimit = PAGE_SIZE;
        this.render(true);
      });
      this.nodes.search.addEventListener("input", () => {
        this.visibleLimit = PAGE_SIZE;
        this.render(true);
      });
      this.nodes.refresh.addEventListener("click", () => this.refresh(true));
      this.nodes.more.addEventListener("click", () => {
        this.visibleLimit += PAGE_SIZE;
        this.render(true);
      });
      this.document.addEventListener("submit", () => {
        window.setTimeout(() => this.syncJob(true), 0);
      });
      this.document.addEventListener("click", () => {
        window.setTimeout(() => this.syncJob(false), 0);
      });
      window.addEventListener("storage", () => this.syncJob(true));
      this.normalizeChapterDownloads();
      const observer = new MutationObserver(() => this.normalizeChapterDownloads());
      observer.observe(this.document.body, { childList: true, subtree: true });
      this.syncJob(true);
      this.schedule();
    }

    required(id) {
      const node = this.document.getElementById(id);
      if (!node) {
        throw new Error(`UI element #${id} is missing`);
      }
      return node;
    }

    readJobID() {
      const input = this.document.getElementById("job-id-input");
      const fromInput = input && typeof input.value === "string"
        ? input.value.trim()
        : "";
      if (fromInput) {
        return fromInput;
      }
      for (const key of STORAGE_KEYS) {
        try {
          const stored = window.localStorage.getItem(key);
          if (stored && stored.trim()) {
            return stored.trim();
          }
        } catch (_) {
          return "";
        }
      }
      return "";
    }

    syncJob(force) {
      const jobID = this.readJobID();
      if (jobID === this.activeJobID && !force) {
        return;
      }
      if (jobID !== this.activeJobID) {
        this.activeJobID = jobID;
        this.snapshot = null;
        this.snapshotSignature = "";
        this.visibleLimit = PAGE_SIZE;
        this.nodes.list.replaceChildren();
      }
      if (!jobID) {
        this.nodes.section.hidden = true;
        return;
      }
      this.nodes.section.hidden = false;
      this.nodes.summary.textContent = "Получаем каталог фрагментов…";
      this.refresh(true);
    }

    schedule() {
      window.clearTimeout(this.timer);
      this.timer = window.setTimeout(async () => {
        this.syncJob(false);
        if (!this.document.hidden && this.activeJobID) {
          await this.refresh(false);
        }
        this.schedule();
      }, POLL_INTERVAL_MS);
    }

    isReviewBusy() {
      const active = this.document.activeElement;
      if (active && this.nodes.list.contains(active)) {
        return true;
      }
      return Array.from(this.nodes.list.querySelectorAll("audio")).some(
        (audio) => !audio.paused,
      );
    }

    async refresh(force) {
      if (!this.activeJobID || this.loading) {
        return;
      }
      if (!force && this.isReviewBusy()) {
        this.pendingRefresh = true;
        this.nodes.refreshed.textContent =
          "Есть обновления — нажмите «Обновить», когда закончите правку или прослушивание.";
        return;
      }

      const requestedJobID = this.activeJobID;
      this.loading = true;
      this.nodes.refresh.disabled = true;
      this.nodes.refresh.classList.add("is-busy");
      this.nodes.list.setAttribute("aria-busy", "true");
      try {
        const response = await this.api.catalog(requestedJobID);
        if (requestedJobID !== this.activeJobID) {
          return;
        }
        const fragments = Array.isArray(response && response.fragments)
          ? response.fragments
          : [];
        const signature = JSON.stringify(
          fragments.map((fragment) => [
            fragment.id,
            fragment.status,
            fragment.attempt,
            fragment.updated_at,
            fragment.text,
            fragment.audio_available,
          ]),
        );
        this.snapshot = { ...response, fragments };
        this.pendingRefresh = false;
        if (force || signature !== this.snapshotSignature) {
          this.snapshotSignature = signature;
          this.render(true);
        } else {
          this.renderSummary(fragments);
        }
        this.nodes.refreshed.textContent =
          `Обновлено ${new Intl.DateTimeFormat("ru-RU", {
            hour: "2-digit",
            minute: "2-digit",
            second: "2-digit",
          }).format(new Date())}`;
      } catch (error) {
        this.nodes.summary.textContent =
          `Не удалось загрузить фрагменты: ${error.message}`;
        this.nodes.refreshed.textContent = "Повторим запрос автоматически.";
      } finally {
        this.loading = false;
        this.nodes.refresh.disabled = false;
        this.nodes.refresh.classList.remove("is-busy");
        this.nodes.list.setAttribute("aria-busy", "false");
      }
    }

    filteredFragments() {
      const fragments = this.snapshot ? this.snapshot.fragments : [];
      const filter = this.nodes.filter.value;
      const query = this.nodes.search.value.trim().toLocaleLowerCase("ru-RU");
      return fragments.filter((fragment) => {
        if (filter === "actionable" && !ACTIONABLE_STATUSES.has(fragment.status)) {
          return false;
        }
        if (filter === "in_progress" &&
          fragment.status !== "pending" && fragment.status !== "generating") {
          return false;
        }
        if (!["all", "actionable", "in_progress"].includes(filter) &&
          fragment.status !== filter) {
          return false;
        }
        if (!query) {
          return true;
        }
        const haystack = [
          fragment.text,
          fragment.stt_text,
          fragment.chapter_title,
          fragment.warning_code,
          fragment.error,
          String(fragment.ordinal || ""),
          String(fragment.chapter_number || ""),
        ].join(" ").toLocaleLowerCase("ru-RU");
        return haystack.includes(query);
      });
    }

    render(force) {
      if (!this.snapshot) {
        return;
      }
      if (!force && this.isReviewBusy()) {
        return;
      }
      const fragments = this.filteredFragments();
      this.renderSummary(this.snapshot.fragments);
      this.nodes.list.replaceChildren();

      const visible = fragments.slice(0, this.visibleLimit);
      const chapters = new Map();
      for (const fragment of visible) {
        const number = Number(fragment.chapter_number) || 0;
        if (!chapters.has(number)) {
          chapters.set(number, []);
        }
        chapters.get(number).push(fragment);
      }
      for (const [number, chapterFragments] of chapters) {
        this.nodes.list.append(this.createChapter(number, chapterFragments));
      }

      const empty = fragments.length === 0;
      this.nodes.empty.hidden = !empty;
      this.nodes.more.hidden = empty || visible.length >= fragments.length;
      if (!this.nodes.more.hidden) {
        this.nodes.more.textContent =
          `Показать ещё (${fragments.length - visible.length})`;
      }
    }

    renderSummary(fragments) {
      const counts = {
        total: fragments.length,
        pending: 0,
        ready: 0,
        warning: 0,
        failed: 0,
      };
      for (const fragment of fragments) {
        if (fragment.status === "ready") counts.ready += 1;
        else if (fragment.status === "warning") counts.warning += 1;
        else if (fragment.status === "failed") counts.failed += 1;
        else counts.pending += 1;
      }
      const completed = counts.ready + counts.warning + counts.failed;
      const percentage = counts.total > 0
        ? Math.round((completed / counts.total) * 100)
        : 0;
      this.nodes.total.textContent = String(counts.total);
      this.nodes.pending.textContent = String(counts.pending);
      this.nodes.ready.textContent = String(counts.ready);
      this.nodes.warning.textContent = String(counts.warning);
      this.nodes.failed.textContent = String(counts.failed);
      this.nodes.progress.value = percentage;
      this.nodes.progress.textContent = `${percentage}%`;
      this.nodes.progressLabel.textContent = `${percentage}%`;
      this.nodes.summary.textContent = counts.total === 0
        ? "В этой задаче пока нет фрагментов."
        : `Готово ${completed} из ${counts.total}. ` +
          `${counts.warning + counts.failed} требуют внимания.`;
    }

    createChapter(number, fragments) {
      const details = this.document.createElement("details");
      details.className = "fragment-chapter";
      details.open = fragments.length <= 12 || fragments.some(
        (fragment) => fragment.status === "warning" || fragment.status === "failed",
      );
      const summary = this.document.createElement("summary");
      const title = this.document.createElement("span");
      title.className = "fragment-chapter-title";
      const strong = this.document.createElement("strong");
      strong.textContent = fragments[0].chapter_title || `Глава ${number}`;
      const small = this.document.createElement("small");
      small.textContent = `${fragments.length} фрагм.`;
      title.append(strong, small);
      const health = this.document.createElement("span");
      health.className = "fragment-chapter-health";
      const issues = fragments.filter(
        (fragment) => fragment.status === "warning" || fragment.status === "failed",
      ).length;
      health.textContent = issues > 0 ? `Проверить: ${issues}` : "Без замечаний";
      summary.append(title, health);
      const list = this.document.createElement("div");
      list.className = "fragment-cards";
      for (const fragment of fragments) {
        list.append(this.createCard(fragment));
      }
      details.append(summary, list);
      return details;
    }

    createCard(fragment) {
      const card = this.document.createElement("article");
      card.className = `fragment-card fragment-card--${fragment.status || "pending"}`;
      card.dataset.fragmentId = fragment.id || "";

      const header = this.document.createElement("header");
      const identity = this.document.createElement("div");
      const ordinal = this.document.createElement("strong");
      ordinal.textContent = `Фрагмент ${Number(fragment.ordinal) || 0}`;
      const meta = this.document.createElement("small");
      const duration = this.formatDuration(fragment.duration_ms);
      meta.textContent = `Попытка ${Number(fragment.attempt) || 0}` +
        (duration ? ` · ${duration}` : "");
      identity.append(ordinal, meta);
      const badge = this.document.createElement("span");
      badge.className = "fragment-status";
      badge.dataset.status = fragment.status || "pending";
      badge.textContent = STATUS_LABELS[fragment.status] || fragment.status || "Ожидает";
      header.append(identity, badge);

      const body = this.document.createElement("div");
      body.className = "fragment-card-body";
      const editor = this.document.createElement("label");
      editor.className = "fragment-editor";
      const editorTitle = this.document.createElement("span");
      editorTitle.textContent = "Текст для озвучивания";
      const textarea = this.document.createElement("textarea");
      textarea.rows = 4;
      textarea.maxLength = 20000;
      textarea.value = fragment.text || "";
      textarea.disabled = !fragment.editable;
      textarea.setAttribute(
        "aria-label",
        `Текст фрагмента ${Number(fragment.ordinal) || 0}`,
      );
      editor.append(editorTitle, textarea);

      const review = this.document.createElement("div");
      review.className = "fragment-review-column";
      if (fragment.audio_available) {
        const audioLabel = this.document.createElement("span");
        audioLabel.className = "fragment-column-label";
        audioLabel.textContent = "Прослушивание";
        const audio = this.document.createElement("audio");
        audio.controls = true;
        audio.preload = "none";
        audio.src = `/v1/fragment/${encodeURIComponent(fragment.id)}/audio.wav`;
        audio.setAttribute(
          "aria-label",
          `Аудио фрагмента ${Number(fragment.ordinal) || 0}`,
        );
        review.append(audioLabel, audio);
      } else {
        const unavailable = this.document.createElement("p");
        unavailable.className = "fragment-audio-unavailable";
        unavailable.textContent = fragment.status === "generating"
          ? "Аудио создаётся."
          : "Готовое аудио пока недоступно.";
        review.append(unavailable);
      }

      const transcript = this.document.createElement("details");
      transcript.className = "fragment-transcript";
      const transcriptSummary = this.document.createElement("summary");
      transcriptSummary.textContent = "Расшифровка STT и диагностика";
      const transcriptText = this.document.createElement("pre");
      transcriptText.textContent =
        fragment.stt_text || fragment.error || "Расшифровка ещё не получена.";
      transcript.append(transcriptSummary, transcriptText);
      review.append(transcript);
      body.append(editor, review);

      const footer = this.document.createElement("footer");
      const diagnostic = this.document.createElement("p");
      diagnostic.className = "fragment-diagnostic";
      diagnostic.textContent = this.diagnostic(fragment);
      footer.append(diagnostic);
      if (fragment.editable) {
        const save = this.document.createElement("button");
        save.type = "button";
        save.className = "button button--secondary fragment-save";
        save.textContent = "Сохранить и переозвучить";
        save.disabled = true;
        const originalText = textarea.value.trim();
        textarea.addEventListener("input", () => {
          save.disabled = textarea.value.trim() === "" ||
            textarea.value.trim() === originalText;
        });
        save.addEventListener("click", () => {
          this.save(fragment, textarea, save);
        });
        footer.append(save);
      }
      card.append(header, body, footer);
      return card;
    }

    diagnostic(fragment) {
      if (fragment.status === "warning") {
        return fragment.warning_code
          ? `Причина проверки: ${fragment.warning_code}`
          : "Фрагмент требует проверки.";
      }
      if (fragment.status === "failed") {
        return fragment.error || "Генерация завершилась ошибкой.";
      }
      if (fragment.status === "ready") {
        return "Можно прослушать и при необходимости исправить текст.";
      }
      return "Состояние обновится автоматически.";
    }

    async save(fragment, textarea, button) {
      const newText = textarea.value.trim();
      if (!newText) {
        textarea.focus();
        return;
      }
      button.disabled = true;
      button.classList.add("is-busy");
      button.textContent = "Сохраняем…";
      try {
        await this.api.edit(fragment.id, newText);
        this.nodes.refreshed.textContent =
          "Правка сохранена. Фрагмент поставлен на переозвучивание.";
        await this.refresh(true);
      } catch (error) {
        if (error instanceof APIError &&
          error.code === "FRAGMENT_SAVED_QUEUE_FULL") {
          this.nodes.refreshed.textContent =
            "Текст сохранён, но очередь заполнена. Повторите фрагмент позже.";
          await this.refresh(true);
        } else {
          this.nodes.refreshed.textContent = `Правка не сохранена: ${error.message}`;
          button.disabled = false;
        }
      } finally {
        button.classList.remove("is-busy");
        button.textContent = "Сохранить и переозвучить";
      }
    }

    normalizeChapterDownloads() {
      const links = this.document.querySelectorAll(
        "#chapter-downloads-list a[href$='/audio.zip']",
      );
      for (const link of links) {
        link.href = link.href.replace(/\/audio\.zip$/, "/audio.flac");
        const filename = String(link.download || "")
          .replace(/\.zip$/i, ".flac")
          .replace(/^ready-chapter-flac-/, "chapter-");
        link.download = filename || "chapter.flac";
        link.title = "Скачать главу одним FLAC-файлом";
        const detail = link.querySelector("small");
        if (detail) {
          detail.textContent = detail.textContent
            .replace(/\.zip/gi, ".flac")
            .replace(/один FLAC в ZIP/gi, "прямой FLAC без архива");
        }
      }
    }

    formatDuration(milliseconds) {
      const value = Number(milliseconds);
      if (!Number.isFinite(value) || value <= 0) {
        return "";
      }
      const seconds = Math.round(value / 1000);
      const minutes = Math.floor(seconds / 60);
      return `${minutes}:${String(seconds % 60).padStart(2, "0")}`;
    }
  }

  window.addEventListener("DOMContentLoaded", () => {
    const section = document.getElementById("fragment-studio");
    if (!section) {
      return;
    }
    const studio = new FragmentStudio(document);
    studio.init();
  });
})();
