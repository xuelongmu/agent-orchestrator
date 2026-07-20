const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { resolve } = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

function loadEnhancement({
	naturalWidth,
	naturalHeight,
	viewportWidth,
	viewportHeight,
	bufferType = "normal",
}) {
	const source = readFileSync(resolve(__dirname, "../app/session/[id].tsx"), "utf8");
	const match = source.match(/const TERMINAL_ENHANCE_JS = `([\s\S]*?)`;\r?\n/);
	assert.ok(match, "terminal enhancement source was not found");

	const listeners = new Map();
	const root = {
		offsetWidth: naturalWidth,
		offsetHeight: naturalHeight,
		style: {},
		dispatchEvent() {},
	};
	const screen = {
		offsetWidth: naturalWidth,
		offsetHeight: naturalHeight,
		getBoundingClientRect: () => ({ top: 0, height: naturalHeight }),
	};
	const host = { clientWidth: viewportWidth, clientHeight: viewportHeight };
	const viewport = { scrollTop: 0 };
	const textarea = { disabled: false, setAttribute() {} };
	const activeBuffer = {
		type: bufferType,
		baseY: 0,
		viewportY: 0,
		cursorX: 40,
		cursorY: 49,
	};
	const terminal = {
		cols: 80,
		rows: 60,
		buffer: { active: activeBuffer },
		modes: { mouseTrackingMode: "none" },
		_core: { viewport: { scrollBarWidth: 15 } },
		onResize() {},
		onCursorMove() {},
		scrollToBottom() {},
		clearSelection() {},
	};
	const document = {
		head: { appendChild() {} },
		body: {},
		createElement: () => ({ textContent: "" }),
		getElementById: (id) => (id === "terminal" ? host : null),
		querySelector: (selector) => {
			if (selector === ".xterm") return root;
			if (selector === ".xterm-screen") return screen;
			if (selector === ".xterm-viewport") return viewport;
			if (selector === ".xterm-helper-textarea") return textarea;
			return null;
		},
		addEventListener(type, listener) {
			const registered = listeners.get(type) || [];
			registered.push(listener);
			listeners.set(type, registered);
		},
	};
	const window = {
		innerWidth: viewportWidth,
		innerHeight: viewportHeight,
		terminal,
		fitAddon: { proposeDimensions: () => ({ cols: 80, rows: 30 }) },
		ReactNativeWebView: { postMessage() {} },
	};
	const context = {
		console,
		document,
		window,
		navigator: {},
		IS_ANDROID: false,
		ResizeObserver: class {
			observe() {}
		},
		setTimeout(callback) {
			callback();
			return 1;
		},
		clearTimeout() {},
		setInterval() {
			return 1;
		},
	};

	vm.runInNewContext(match[1], context);
	// Initialization timers run eagerly above; gesture timers stay pending so a
	// synthetic drag does not also become a synthetic long press.
	context.setTimeout = () => 1;
	return { listeners, root };
}

function pinch(listeners, startDistance, endDistance) {
	const [touchStart] = listeners.get("touchstart");
	const [touchMove] = listeners.get("touchmove");
	const [touchEnd] = listeners.get("touchend");
	const touches = (distance) => [
		{ clientX: 160 - distance / 2, clientY: 300 },
		{ clientX: 160 + distance / 2, clientY: 300 },
	];
	touchStart({ touches: touches(startDistance) });
	touchMove({ touches: touches(endDistance), cancelable: true, preventDefault() {} });
	touchEnd({ touches: [] });
}

function dragIsIntercepted(listeners) {
	const [touchStart] = listeners.get("touchstart");
	const [touchMove] = listeners.get("touchmove");
	touchStart({ touches: [{ clientX: 100, clientY: 300 }] });
	touchMove({ touches: [{ clientX: 100, clientY: 280 }] });
	let prevented = false;
	touchMove({
		touches: [{ clientX: 100, clientY: 250 }],
		cancelable: true,
		preventDefault() {
			prevented = true;
		},
	});
	return prevented;
}

function tap(listeners, clientX = 160, clientY = 300) {
	const [touchStart] = listeners.get("touchstart");
	const [touchEnd] = listeners.get("touchend");
	touchStart({ touches: [{ clientX, clientY }] });
	touchEnd({ touches: [] });
}

test("height-only authoritative overflow frames the cursor and routes drags to pan", () => {
	const { listeners, root } = loadEnhancement({
		naturalWidth: 300,
		naturalHeight: 1200,
		viewportWidth: 320,
		viewportHeight: 600,
	});

	// Row 49 is centered at y=990. Framing keeps it at the 48px lower margin,
	// without shrinking the fit-to-width scale below 1:1.
	assert.equal(root.style.transform, "translate(0px,-438px) scale(1)");

	assert.equal(dragIsIntercepted(listeners), true);
	assert.equal(root.style.transform, "translate(0px,-468px) scale(1)");
});

test("a phone-sized grid retains native scroll gesture routing", () => {
	const { listeners, root } = loadEnhancement({
		naturalWidth: 300,
		naturalHeight: 600,
		viewportWidth: 320,
		viewportHeight: 600,
	});

	assert.equal(root.style.transform, "translate(0px,0px) scale(1)");
	assert.equal(dragIsIntercepted(listeners), false);
	assert.equal(root.style.transform, "translate(0px,0px) scale(1)");
});

test("width overflow pinch-out retains the fit-to-width scale", () => {
	const { listeners, root } = loadEnhancement({
		naturalWidth: 800,
		naturalHeight: 600,
		viewportWidth: 320,
		viewportHeight: 600,
	});

	pinch(listeners, 100, 20);
	assert.equal(root.style.transform, "translate(0px,0px) scale(0.4)");
	assert.equal(dragIsIntercepted(listeners), false);
});

test("height-only pinch-out selects overview with native scroll routing", () => {
	const { listeners, root } = loadEnhancement({
		naturalWidth: 300,
		naturalHeight: 1200,
		viewportWidth: 320,
		viewportHeight: 600,
	});

	pinch(listeners, 100, 50);
	assert.equal(root.style.transform, "translate(0px,0px) scale(1)");
	assert.equal(dragIsIntercepted(listeners), false);
});

test("height-only pinch-in returns to cursor-framed pan routing", () => {
	const { listeners, root } = loadEnhancement({
		naturalWidth: 300,
		naturalHeight: 1200,
		viewportWidth: 320,
		viewportHeight: 600,
	});

	pinch(listeners, 100, 50);
	pinch(listeners, 50, 100);
	assert.equal(root.style.transform, "translate(0px,-438px) scale(1)");
	assert.equal(dragIsIntercepted(listeners), true);
});

test("height-only double-tap still returns from overview to cursor-framed pan", () => {
	const { listeners, root } = loadEnhancement({
		naturalWidth: 300,
		naturalHeight: 1200,
		viewportWidth: 320,
		viewportHeight: 600,
	});

	pinch(listeners, 100, 50);
	tap(listeners);
	tap(listeners);
	assert.equal(root.style.transform, "translate(0px,-438px) scale(1)");
	assert.equal(dragIsIntercepted(listeners), true);
});

test("height-only alternate-screen drags pan before routing overshoot to the app", () => {
	const { listeners, root } = loadEnhancement({
		naturalWidth: 300,
		naturalHeight: 1200,
		viewportWidth: 320,
		viewportHeight: 600,
		bufferType: "alternate",
	});

	assert.equal(dragIsIntercepted(listeners), true);
	assert.equal(root.style.transform, "translate(0px,-468px) scale(1)");
});
