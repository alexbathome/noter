// noter shell.
//
// Everything about *tasks* — creating, moving, selecting, rendering titles —
// lives on the Go server and arrives as HTML over Datastar's SSE stream. This
// file is only the parts a hypermedia round-trip genuinely cannot do:
//
//   1. probing whether noter is running before booting the app,
//   2. the Monaco editor island (which the server drives via window.noter),
//   3. HTML5 drag-and-drop, which needs local pointer state mid-gesture.

(function () {
    'use strict';

    var PORT = new URLSearchParams(location.search).get('port') || '11911';
    var BASE = 'http://localhost:' + PORT;

    var MONACO = 'https://cdn.jsdelivr.net/npm/monaco-editor@0.52.2/min/vs';
    var MONACO_VIM = 'https://cdn.jsdelivr.net/npm/monaco-vim@0.4.4/dist/monaco-vim.umd';
    var DATASTAR = 'https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.2/bundles/datastar.js';

    // ---------------------------------------------------------------- boot

    // A page served from GitHub Pages over HTTPS may not be allowed to reach
    // loopback at all: Safari refuses outright, and Chrome 142+ gates it behind
    // the Local Network Access permission prompt. Any failure means the same
    // thing to us — show the instructions.
    fetch(BASE + '/healthz', { mode: 'cors', cache: 'no-store' })
        .then(function (r) { return r.ok ? r.json() : Promise.reject(r.status); })
        .then(function (info) {
            if (info && info.app === 'noter') { boot(); } else { Promise.reject('unexpected'); }
        })
        .catch(function () { /* offline panel is already the default view */ });

    function boot() {
        document.getElementById('offline').hidden = true;
        document.getElementById('app').hidden = false;

        // Point the declarative bits at the port we actually found.
        document.getElementById('zones').setAttribute(
            'data-init', "@get('" + BASE + "/api/board', {openWhenHidden: true})");
        document.getElementById('delete').setAttribute(
            'data-on:click', "@delete('" + BASE + "/api/tasks/' + $sel)");

        loadEditor();

        // Datastar scans the DOM when it loads, so it goes last.
        var s = document.createElement('script');
        s.type = 'module';
        s.src = DATASTAR;
        document.head.appendChild(s);
    }

    // ------------------------------------------------------- editor island

    var editor, emptyModel, current = null, saveTimer = null;
    var models = Object.create(null); // task id -> monaco model, for undo isolation

    function loadEditor() {
        var loader = document.createElement('script');
        loader.src = MONACO + '/loader.js';
        loader.onload = function () {
            require.config({ paths: { vs: MONACO, 'monaco-vim': MONACO_VIM } });
            require(['vs/editor/editor.main'], function () {
                var darkMode = window.matchMedia('(prefers-color-scheme: dark)');
                emptyModel = monaco.editor.createModel('', 'markdown');

                editor = monaco.editor.create(document.getElementById('editor'), {
                    model: emptyModel,
                    automaticLayout: true,
                    readOnly: true,
                    minimap: { enabled: false },
                    theme: darkMode.matches ? 'vs-dark' : 'vs',
                    fontFamily: 'SpaceMono, monospace',
                    fontLigatures: true
                });

                document.fonts.ready.then(function () { monaco.editor.remeasureFonts(); });
                darkMode.onchange = function (e) {
                    monaco.editor.setTheme(e.matches ? 'vs-dark' : 'vs');
                };

                editor.onDidChangeModelContent(scheduleSave);

                // monaco-vim's UMD build asks the AMD loader for the ESM api module
                // id; point it at the global the min/vs build already installed.
                define('monaco-editor/esm/vs/editor/editor.api', [], function () { return window.monaco; });
                require(['monaco-vim'], function (MonacoVim) {
                    MonacoVim.initVimMode(editor, document.getElementById('status'));
                    MonacoVim.VimMode.Vim.defineEx('write', 'w', flushSave);
                });
            });
        };
        document.head.appendChild(loader);
    }

    // The server calls these directly over SSE when the selection changes.
    window.noter = {
        load: function (id, content) {
            if (!editor) { return; }
            flushSave();
            if (!models[id]) {
                models[id] = monaco.editor.createModel(content, 'markdown');
            } else if (models[id].getValue() !== content) {
                models[id].setValue(content);
            }
            current = id;
            editor.setModel(models[id]);
            editor.updateOptions({ readOnly: false });
            editor.focus();
        },
        clear: function () {
            if (!editor) { return; }
            clearTimeout(saveTimer);
            saveTimer = null;
            if (current && models[current]) {
                models[current].dispose();
                delete models[current];
            }
            current = null;
            editor.setModel(emptyModel);
            editor.updateOptions({ readOnly: true });
        }
    };

    // Debounced so a burst of typing is one write, not one per keystroke. The
    // board title updates come back on the SSE stream.
    function scheduleSave() {
        if (!current) { return; }
        clearTimeout(saveTimer);
        saveTimer = setTimeout(flushSave, 400);
    }

    function flushSave() {
        clearTimeout(saveTimer);
        saveTimer = null;
        if (!current || !models[current]) { return; }
        // text/plain keeps this a CORS-simple request, so it skips the preflight.
        fetch(BASE + '/api/tasks/' + encodeURIComponent(current), {
            method: 'PUT',
            headers: { 'Content-Type': 'text/plain' },
            body: models[current].getValue()
        }).catch(function () { /* the stream will resync on reconnect */ });
    }

    window.addEventListener('beforeunload', flushSave);

    // ------------------------------------------------------------ dragging
    //
    // Delegated from document, because #zones is replaced by the server on
    // every board change and per-element handlers would not survive the morph.

    var dragging = null;

    document.addEventListener('dragstart', function (e) {
        var task = e.target.closest && e.target.closest('.task');
        if (!task) { return; }
        dragging = task;
        e.dataTransfer.setData('text/plain', task.dataset.id);
        e.dataTransfer.effectAllowed = 'move';
        setTimeout(function () { task.classList.add('dragging'); });
    });

    document.addEventListener('dragend', function () {
        if (dragging) { dragging.classList.remove('dragging'); }
        dragging = null;
        document.querySelectorAll('.list.over').forEach(function (l) {
            l.classList.remove('over');
        });
    });

    document.addEventListener('dragover', function (e) {
        var list = e.target.closest && e.target.closest('.list');
        if (!list || !dragging) { return; }
        e.preventDefault();
        list.classList.add('over');
        list.insertBefore(dragging, dropBefore(list, e.clientY));
    });

    document.addEventListener('dragleave', function (e) {
        var list = e.target.closest && e.target.closest('.list');
        if (list) { list.classList.remove('over'); }
    });

    document.addEventListener('drop', function (e) {
        var list = e.target.closest && e.target.closest('.list');
        if (!list || !dragging) { return; }
        e.preventDefault();
        list.classList.remove('over');

        // The element is already in place from the dragover preview, so its
        // index here is the index we want the server to persist.
        var id = dragging.dataset.id;
        var index = Array.prototype.indexOf.call(list.querySelectorAll('.task'), dragging);
        fetch(BASE + '/api/tasks/' + encodeURIComponent(id) + '/move'
            + '?zone=' + encodeURIComponent(list.dataset.zone) + '&index=' + index,
            { method: 'POST' })
            .catch(function () { /* the stream will resync on reconnect */ });
    });

    function dropBefore(list, y) {
        var tasks = list.querySelectorAll('.task:not(.dragging)');
        for (var i = 0; i < tasks.length; i++) {
            var box = tasks[i].getBoundingClientRect();
            if (y < box.top + box.height / 2) { return tasks[i]; }
        }
        return null;
    }
})();
