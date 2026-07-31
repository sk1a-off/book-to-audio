(() => {
  "use strict";

  const PAGE_SIZE = 20;
  const POLL_INTERVAL_MS = 5000;
  const STATUS_LABELS = Object.freeze({
    pending: "Ожидает",
    generating: "Озвучивается",
    ready: "Готов",
    warning: "Проверить",
    failed: "Ошибка",
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
        try { problem = await response.json(); } catch (_) { /* proxy error */ }
        throw new APIError(
          response.status,
          problem && problem.code,
          problem && problem.error ? problem.error : `Запрос завершился с кодом ${response.status}`,
        );
      }
      if (response.status === 204) return null;
      return response.json();
    }

    chapter(jobID, chapterNumber) {
      return this.request(
        `/v1/job/${encodeURIComponent(jobID)}/chapters/${encodeURIComponent(chapterNumber)}`,
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
      return this.request(`/v1/job/${encodeURIComponent(jobID)}/retry/warnings`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ fragment_ids: [fragmentID] }),
      });
    }

    approve(fragmentID) {
      return this.request(`/v1/fragment/${encodeURIComponent(fragmentID)}/approve`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ reason: "Проверено на странице главы" }),
      });
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
      };
    }

    required(id) {
      const node = this.document.getElementById(id);
      if (!node) throw new Error(`UI element #${id} is missing`);
      return node;
    }

    init() {
      const match = window.location.pathname.match(/^\/jobs\/([^/]+)\/chapters\/(\d+)$/);
      if (!match) {
        this.failPage("Некорректный адрес главы.");
        return;
      }
      this.jobID = decodeURIComponent(match[1]);
      this.chapterNumber = Number(match[2]);
      this.nodes.refresh.addEventListener("click", () => this.refresh(true));
      this.nodes.filter.addEventListener("change", () => { this.page = 0; this.renderFragments(); });
      this.nodes.search.addEventListener("input", () => { this.page = 0; this.renderFragments(); });
      this.nodes.previous.addEventListener("click", () => {
        if (this.page > 0) { this.page -= 1; this.renderFragments(); }
      });
      this.nodes.next.addEventListener("click", () => {
        const pages = this.pageCount();
        if (this.page + 1 < pages) { this.page += 1; this.renderFragments(); }
      });
      this.nodes.download.addEventListener("click", (event) => {
        if (this.nodes.download.getAttribute("aria-disabled") === "true") {
          event.preventDefault();
          this.notify("Скачивание появится, когда у каждого фрагмента будет аудио.", "error");
        }
      });
      this.refresh(true);
      this.schedule();
    }

    isReviewBusy() {
      const active = this.document.activeElement;
      if (active && this.nodes.list.contains(active) &&
          (active.tagName === "TEXTAREA" || active.tagName === "INPUT")) return true;
      return Array.from(this.nodes.list.querySelectorAll("audio")).some((audio) => !audio.paused);
    }

    schedule() {
      window.clearTimeout(this.timer);
      this.timer = window.setTimeout(async () => {
        if (!this.document.hidden && !this.isReviewBusy() && this.busy.size === 0) {
          await this.refresh(false);
        } else if (this.isReviewBusy()) {
          this.nodes.refreshState.textContent = "Автообновление приостановлено на время правки или прослушивания.";
        }
        this.schedule();
      }, POLL_INTERVAL_MS);
    }

    async refresh(announce) {
      if (this.busy.has("refresh")) return;
      this.busy.add("refresh");
      this.nodes.refresh.disabled = true;
      this.nodes.refresh.classList.add("is-busy");
      try {
        const snapshot = await this.api.chapter(this.jobID, this.chapterNumber);
        this.snapshot = snapshot;
        this.render();
        this.nodes.refreshState.textContent = `Обновлено ${new Intl.DateTimeFormat("ru-RU", {
          hour: "2-digit", minute: "2-digit", second: "2-digit",
        }).format(new Date())}`;
        if (announce) this.notify("Данные главы обновлены.");
      } catch (error) {
        this.failPage(error.message || "Не удалось загрузить главу.");
        if (announce) this.notify(error.message, "error");
      } finally {
        this.busy.delete("refresh");
        this.nodes.refresh.disabled = false;
        this.nodes.refresh.classList.remove("is-busy");
      }
    }

    render() {
      const chapter = this.snapshot && this.snapshot.chapter;
      if (!chapter) return;
      const title = chapter.title || `Глава ${chapter.chapter_number}`;
      this.document.title = `${title} — Голос Книги`;
      this.nodes.title.textContent = title;
      this.nodes.subtitle.textContent = `Задача ${this.snapshot.job_id} · глава ${chapter.chapter_number}`;
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
        ? review > 0 ? "Озвучено · есть замечания" : "Готово"
        : "Озвучивание продолжается";
      this.nodes.summary.textContent = chapter.audio_complete
        ? review > 0
          ? `Все ${total} фрагментов имеют аудио. ${review} можно проверить, но скачивание уже доступно.`
          : `Все ${total} фрагментов озвучены без замечаний.`
        : `Озвучено ${voiced} из ${total}. Страница показывает только одну главу и не перегружает основной интерфейс.`;
      if (chapter.audio_complete && chapter.audio_url) {
        this.nodes.download.href = chapter.audio_url;
        this.nodes.download.download = chapter.audio_filename || `chapter-${chapter.chapter_number}.flac`;
        this.nodes.download.classList.remove("is-disabled");
        this.nodes.download.setAttribute("aria-disabled", "false");
      } else {
        this.nodes.download.removeAttribute("href");
        this.nodes.download.removeAttribute("download");
        this.nodes.download.classList.add("is-disabled");
        this.nodes.download.setAttribute("aria-disabled", "true");
      }
      this.renderFragments();
    }

    filteredFragments() {
      const fragments = this.snapshot && Array.isArray(this.snapshot.fragments)
        ? this.snapshot.fragments : [];
      const filter = this.nodes.filter.value;
      const query = this.nodes.search.value.trim().toLocaleLowerCase("ru-RU");
      return fragments.filter((fragment) => {
        if (filter === "issues" && fragment.status !== "warning" && fragment.status !== "failed") return false;
        if (filter === "ready" && fragment.status !== "ready") return false;
        if (filter === "in_progress" && fragment.status !== "pending" && fragment.status !== "generating") return false;
        if (query) {
          const haystack = [fragment.text, fragment.stt_text, fragment.warning_code,
            fragment.error, String(fragment.ordinal || "")]
            .join(" ").toLocaleLowerCase("ru-RU");
          if (!haystack.includes(query)) return false;
        }
        return true;
      }).sort((left, right) => this.number(left.ordinal) - this.number(right.ordinal));
    }

    pageCount() {
      return Math.max(1, Math.ceil(this.filteredFragments().length / PAGE_SIZE));
    }

    renderFragments() {
      if (!this.snapshot) return;
      const fragments = this.filteredFragments();
      const pages = Math.max(1, Math.ceil(fragments.length / PAGE_SIZE));
      if (this.page >= pages) this.page = pages - 1;
      const visible = fragments.slice(this.page * PAGE_SIZE, (this.page + 1) * PAGE_SIZE);
      this.nodes.list.replaceChildren();
      for (const fragment of visible) this.nodes.list.append(this.createCard(fragment));
      this.nodes.empty.hidden = fragments.length !== 0;
      this.nodes.previous.disabled = this.page === 0;
      this.nodes.next.disabled = this.page + 1 >= pages;
      this.nodes.pageLabel.textContent = `Страница ${this.page + 1} из ${pages} · ${fragments.length} фрагм.`;
    }

    createCard(fragment) {
      const card = this.create("article", "fragment-card");
      card.dataset.status = fragment.status || "pending";
      const header = this.create("header");
      const identity = this.create("div");
      identity.append(
        this.create("strong", "", `Фрагмент ${this.number(fragment.ordinal)}`),
        this.create("small", "", `Попыток: ${this.number(fragment.attempt)} · Whisper: ${this.scoreLabel(fragment.transcript_score)}`),
      );
      const badge = this.create("span", "fragment-status", STATUS_LABELS[fragment.status] || fragment.status || "Ожидает");
      header.append(identity, badge);

      const body = this.create("div", "fragment-body");
      const editor = this.create("label", "fragment-editor");
      editor.append(this.create("span", "", "Текст для озвучивания"));
      const textarea = this.create("textarea");
      textarea.value = fragment.text || "";
      textarea.maxLength = 20000;
      textarea.readOnly = !fragment.editable;
      textarea.setAttribute("aria-label", `Текст фрагмента ${this.number(fragment.ordinal)}`);
      editor.append(textarea);

      const recognition = this.create("div", "recognition");
      recognition.append(this.create("span", "", "Распознано Whisper"));
      recognition.append(this.create("pre", "", fragment.stt_text || "Расшифровка ещё не получена."));
      recognition.append(this.create("span", "score", `Сходство: ${this.scoreLabel(fragment.transcript_score)}`));
      if (fragment.audio_available) {
        const audio = this.create("audio");
        audio.controls = true;
        audio.preload = "none";
        audio.src = `/v1/fragment/${encodeURIComponent(fragment.id)}/audio.wav`;
        recognition.append(audio);
      }
      if (fragment.error) recognition.append(this.create("p", "fragment-error", fragment.error));
      body.append(editor, recognition);

      const actions = this.create("div", "fragment-actions");
      if (fragment.editable) {
        const save = this.button("Сохранить и переозвучить");
        save.addEventListener("click", () => this.edit(fragment, textarea, save));
        actions.append(save);
      }
      if (fragment.status === "warning" || fragment.status === "failed") {
        const retry = this.button("Повторить только этот фрагмент");
        retry.addEventListener("click", () => this.retry(fragment, retry));
        actions.append(retry);
      }
      if (fragment.status === "warning" && fragment.audio_available) {
        const approve = this.button("Принять текущее аудио");
        approve.addEventListener("click", () => this.approve(fragment, approve));
        actions.append(approve);
      }
      card.append(header, body, actions);
      return card;
    }

    async edit(fragment, textarea, button) {
      const text = textarea.value.trim();
      if (!text) { this.notify("Текст не может быть пустым.", "error"); textarea.focus(); return; }
      await this.runAction(`edit-${fragment.id}`, button, async () => {
        await this.api.edit(fragment.id, text);
        this.notify("Правка сохранена; фрагмент поставлен в очередь.");
      });
    }

    async retry(fragment, button) {
      await this.runAction(`retry-${fragment.id}`, button, async () => {
        await this.api.retry(this.jobID, fragment.id);
        this.notify("Один фрагмент поставлен на повтор. Будет сохранена лучшая попытка по Whisper.");
      });
    }

    async approve(fragment, button) {
      await this.runAction(`approve-${fragment.id}`, button, async () => {
        await this.api.approve(fragment.id);
        this.notify("Текущее аудио принято без повторной генерации.");
      });
    }

    async runAction(key, button, operation) {
      if (this.busy.has(key)) return;
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
        button.disabled = false;
        button.classList.remove("is-busy");
      }
    }

    scoreLabel(value) {
      const score = Number(value);
      if (!Number.isFinite(score)) return "нет оценки";
      return `${Math.round(Math.max(0, Math.min(1, score)) * 100)}%`;
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
      if (className) node.className = className;
      if (text) node.textContent = text;
      return node;
    }

    notify(message, kind = "success") {
      window.clearTimeout(this.toastTimer);
      this.nodes.toast.textContent = message || "Готово";
      this.nodes.toast.dataset.kind = kind;
      this.nodes.toast.hidden = false;
      this.toastTimer = window.setTimeout(() => { this.nodes.toast.hidden = true; }, 4500);
    }

    failPage(message) {
      this.nodes.status.textContent = "Ошибка";
      this.nodes.summary.textContent = message;
      this.nodes.refreshState.textContent = "Проверьте адрес или вернитесь к обзору задачи.";
    }
  }

  document.addEventListener("DOMContentLoaded", () => {
    try { new ChapterPage(document).init(); }
    catch (error) { console.error("chapter UI initialization failed", error); }
  });
})();
