# Generated quality samples

Сюда помещаются только controlled samples, созданные из `corpus.json` и сопровождаемые manifest по `../manifests/schema.json`.

До выбора Git LFS или внешнего artifact storage большие WAV/FLAC-файлы не следует добавлять в обычную Git history. Metadata, hashes и blind reveal mapping должны оставаться version-controlled.

Для локального controlled preview используйте `book-text-editor/cmd/quality-runner`.
Каждый `experiment_id` получает отдельный каталог; повторное использование ID
завершается контролируемой ошибкой вместо перезаписи результатов.

Для слепого сравнения двух segmentation experiments используйте
`book-text-editor/cmd/quality-blind`. Reveal mapping хранится отдельно от
страницы прослушивания и не должен открываться до сохранения оценки.
