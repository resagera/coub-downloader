# Coub Downloader

Утилита на Go для скачивания медиа с `coub.com` через официальный API.

Поддерживает:
- скачивание лучшей аудиодорожки
- скачивание share-видео
- скачивание лучшего видео
- скачивание картинки (`picture`)
- кэширование ответа API в `cache/`
- пакетную обработку ссылок через `--follow`
- объединение видео и аудио через `ffmpeg` в режиме `--compare`
- добавление обложки в итоговый файл при помощи `ffmpeg`, если это возможно

---

## Возможности

### Скачивание через API
Программа:
1. принимает ссылку вида `https://coub.com/view/<permalink>`
2. получает `permalink`
3. проверяет наличие кэша `cache/<permalink>.json`
4. если кэша нет, запрашивает API Coub
5. выбирает нужные медиафайлы
6. сохраняет их в указанную папку

### Поддерживаемые режимы
- `--audio` — скачать лучшую аудиодорожку
- `--share` — скачать `file_versions.share.default`
- `--video` — скачать лучшее видео
- `--picture` — скачать картинку из поля `picture`
- `--all` — скачать всё сразу
- `--compare` — скачать аудио + видео + картинку и собрать итоговый mp4 через `ffmpeg`

### Follow-режим
При запуске с `--follow` программа:
- ждёт ввод ссылок построчно из stdin
- после нажатия Enter ставит задачу на скачивание
- одновременно продолжает ждать следующую ссылку
- обрабатывает несколько ссылок параллельно
- завершает работу по `Ctrl+C`

---

## Требования

### Go
Нужен Go 1.21+ или новее.

### ffmpeg
Нужен только для режима `--compare`.

Проверка:
```bash
ffmpeg -version
````

Если `ffmpeg` не установлен:

#### Ubuntu / Debian

```bash
sudo apt update
sudo apt install ffmpeg
```

#### macOS (Homebrew)

```bash
brew install ffmpeg
```

#### Windows (winget)

```bash
winget install Gyan.FFmpeg
```

---

## Сборка

```bash
go build -o coub-downloader main.go
```

Запуск без сборки:

```bash
go run main.go "https://coub.com/view/4arj3v"
```

---

## Использование

### Базовый синтаксис

Обычный режим:

```bash
./coub-downloader [flags] <coub_url>
```

Follow-режим:

```bash
./coub-downloader --follow [flags]
```

---

## Флаги

### Основные

* `--dir` — папка для скачанных файлов
  по умолчанию: `music`

* `--cache` — папка для JSON-кэша API
  по умолчанию: `cache`

* `--force` — игнорировать существующий кэш и заново скачивать файлы

* `--debug` — включить подробные отладочные логи

* `--timeout` — таймаут HTTP-запросов
  по умолчанию: `30s`

### Параллельный режим

* `--follow` — читать ссылки из stdin и скачивать параллельно
* `--workers` — количество воркеров в `--follow` режиме
  по умолчанию: `3`

### Режимы скачивания

* `--audio` — скачать лучшую аудиодорожку
* `--share` — скачать share-видео
* `--video` — скачать лучшее видео
* `--picture` — скачать картинку
* `--all` — эквивалент `--audio --share --video --picture`
* `--compare` — скачать аудио + видео + картинку и собрать итоговый mp4

---

## Поведение по умолчанию

Если не указан ни один из флагов:

* `--audio`
* `--share`
* `--video`
* `--picture`
* `--all`
* `--compare`

то программа работает как:

```bash
--audio
```

То есть скачивает только лучшую аудиодорожку.

---

## Примеры

### Скачать только аудио

```bash
./coub-downloader "https://coub.com/view/4arj3v"
```

или явно:

```bash
./coub-downloader --audio "https://coub.com/view/4arj3v"
```

### Скачать share-видео

```bash
./coub-downloader --share "https://coub.com/view/4arj3v"
```

### Скачать лучшее видео и картинку

```bash
./coub-downloader --video --picture "https://coub.com/view/4arj3v"
```

### Скачать всё

```bash
./coub-downloader --all "https://coub.com/view/4arj3v"
```

### Скачать всё в свою папку

```bash
./coub-downloader --all --dir media "https://coub.com/view/4arj3v"
```

### Игнорировать кэш и скачать заново

```bash
./coub-downloader --audio --force "https://coub.com/view/4arj3v"
```

### Собрать итоговый mp4 через ffmpeg

```bash
./coub-downloader --compare "https://coub.com/view/4arj3v"
```

### Follow-режим

```bash
./coub-downloader --follow --all
```

После запуска просто вставляй ссылки по одной в строку:

```text
https://coub.com/view/4arj3v
https://coub.com/view/xxxxxx
https://coub.com/view/yyyyyy
```

### Follow-режим с несколькими воркерами

```bash
./coub-downloader --follow --compare --workers 5
```

---

## Формат имен файлов

Базовое имя строится так:

```text
<sanitized_title>_<permalink>
```

Примеры:

```text
Hells Club_4arj3v.mp3
Hells Club_4arj3v.mp4
Hells Club_4arj3v.jpg
HellS Club_4arj3v_share.mp4
Hells Club_4arj3v_compare.mp4
```

### Что именно создаётся

* аудио:
  `<title>_<permalink>.mp3`

* лучшее видео:
  `<title>_<permalink>.mp4`

* картинка:
  `<title>_<permalink>.<ext>`

* share-видео:
  `<title>_<permalink>_share.mp4`

* compare-файл:
  `<title>_<permalink>_compare.mp4`

---

## Как работает `--compare`

Режим `--compare`:

1. скачивает аудио
2. скачивает лучшее видео
3. скачивает картинку
4. запускает внешний `ffmpeg`
5. лупит видео до длины аудио
6. обрезает результат по длине аудио
7. пытается добавить картинку как cover art

### Важные детали

* видеодорожка дублируется циклически, пока не закончится аудио
* длина итогового файла равна длине аудио
* `ffmpeg` вызывается как внешняя CLI-программа
* программа не зависит от встроенных Go-библиотек для кодирования видео

---

## Кэш

Ответ API сохраняется в:

```text
cache/<permalink>.json
```

Если кэш уже существует и не указан `--force`, повторный запрос к API не делается.

Это ускоряет повторные запуски и уменьшает количество сетевых запросов.

---

## Логи

Используются следующие типы логов:

* `[INFO]` — информационные сообщения
* `[OK]` — файл успешно скачан или создан
* `[WARNING]` — файл уже существует, пустой ввод, дубликат ссылки, некорректный ввод
* `[ERROR]` — ошибка обработки

Пример:

```text
https://coub.com/view/4arj3v
[INFO] worker-4: start https://coub.com/view/4arj3v
[OK] music/Hells Club_4arj3v.mp3
done
permalink: 4arj3v
title: Hells Club
audio: https://...
json:  cache/4arj3v.json
mp3:   music/Hells Club_4arj3v.mp3
[INFO] worker-4: done https://coub.com/view/4arj3v
```

---

## Поведение при повторном скачивании

Если файл уже существует и не указан `--force`, программа:

* не перезаписывает файл
* пишет `[WARNING] already exists: ...`

Если указан `--force`, файл скачивается заново.

---

## Поведение в `--follow`

В режиме `--follow`:

* учитываются все выбранные флаги (`--audio`, `--video`, `--picture`, `--share`, `--all`, `--compare`)
* одна и та же ссылка не ставится в обработку повторно, пока она уже скачивается
* можно обрабатывать сразу несколько ссылок параллельно

---

## Возможные ошибки

### Некорректная ссылка

Пример:

```text
[WARNING] bad url: xxx | err=expected URL like https://coub.com/view/<permalink>
```

### Нет `ffmpeg` при `--compare`

Пример:

```text
[ERROR] ffmpeg not found in PATH. install it first.
Ubuntu/Debian: sudo apt update && sudo apt install ffmpeg
macOS (Homebrew): brew install ffmpeg
Windows (winget): winget install Gyan.FFmpeg
```

### API не вернул нужное поле

Например:

* нет `picture`
* нет `file_versions.share.default`
* не найдены аудио/видео URL

Тогда программа завершится с `[ERROR]`.

---

## Пример структуры папок

После нескольких запусков структура может выглядеть так:

```text
.
├── cache
│   ├── 4arj3v.json
│   ├── abc123.json
│   └── xyz789.json
├── music
│   ├── Hells Club_4arj3v.mp3
│   ├── Hells Club_4arj3v.mp4
│   ├── Hells Club_4arj3v.jpg
│   ├── Hells Club_4arj3v_share.mp4
│   └── Hells Club_4arj3v_compare.mp4
└── coub-downloader
```

---

## Идеи для дальнейших улучшений

В будущем можно добавить:

* сохранение промежуточных файлов ffmpeg
* ограничение длины имени файла
* JSON-режим вывода
* отдельный лог-файл
* файл со списком ссылок вместо stdin
* дедупликацию уже скачанных permalink между запусками

---

## Лицензия

Используй как основу для своего проекта.

```

Если хочешь, я могу сразу подготовить ещё и короткий `README_ru.md` в более “проектном” стиле: с оглавлением, блоком “быстрый старт” и секцией “архитектура работы программы”.
```

