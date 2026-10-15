import React, {act} from 'react';
import {createRoot} from 'react-dom/client';
import {getInteractiveTerminalAppendIndex, ResponseDisplayInteractive} from './ResponseDisplayInteractive';

jest.mock('./CallbackMutations', () => ({
    createTaskingMutation: "createTaskingMutation",
}));
jest.mock('../../utilities/Snackbar', () => ({
    snackActions: {error: jest.fn(), warning: jest.fn()},
}));
jest.mock('./ResponseDisplay', () => ({
    b64DecodeUnicode: (value) => Buffer.from(value, "base64").toString("utf8"),
    SearchBar: () => null,
}));
jest.mock('@xterm/xterm/css/xterm.css', () => ({}));
jest.mock('@apollo/client', () => {
    const actual = jest.requireActual('@apollo/client');
    const mockApollo = {lazy: {}, subscription: null, mutations: []};
    mockApollo.mutate = (args) => mockApollo.mutations.push(args);
    global.mockApollo = mockApollo;
    return {
        ...actual,
        useLazyQuery: (query, options) => {
            const name = query.definitions[0].name.value;
            if(!mockApollo.lazy[name]){
                const entry = {calls: []};
                entry.execute = (args) => entry.calls.push(args);
                mockApollo.lazy[name] = entry;
            }
            mockApollo.lazy[name].options = options;
            return [mockApollo.lazy[name].execute];
        },
        useSubscription: (query, options) => {
            mockApollo.subscription = options;
            return {};
        },
        useMutation: () => [mockApollo.mutate],
    };
});
jest.mock('@xterm/addon-fit', () => ({
    FitAddon: class {
        activate(){}
        dispose(){}
        fit(){}
        proposeDimensions(){ return {cols: 100, rows: 30}; }
    },
}));
jest.mock('@xterm/xterm', () => {
    const actual = jest.requireActual('@xterm/xterm');
    // jsdom has no canvas, so skip rendering but keep the real parser and buffers
    class MockTerminal extends actual.Terminal {
        constructor(options){
            super(options);
            global.mockTerminal = this;
        }
        open(){}
        get textarea(){ return global.document.createElement("textarea"); }
    }
    return {Terminal: MockTerminal};
});

describe("getInteractiveTerminalAppendIndex", () => {
    test("appends everything when nothing has been written yet", () => {
        expect(getInteractiveTerminalAppendIndex([], ["a", "b"])).toBe(0);
    });

    test("appends only new entries when previous is a prefix", () => {
        expect(getInteractiveTerminalAppendIndex(["a", "b"], ["a", "b", "c", "d"])).toBe(2);
    });

    test("appends nothing when unchanged", () => {
        expect(getInteractiveTerminalAppendIndex(["a", "b"], ["a", "b"])).toBe(2);
    });

    test("appends new entries from a sliding window whose head was dropped", () => {
        expect(getInteractiveTerminalAppendIndex(["a", "b", "c"], ["b", "c", "d", "e"])).toBe(2);
    });

    test("requires a replay when the window jumped past everything written", () => {
        expect(getInteractiveTerminalAppendIndex(["a", "b"], ["x", "y"])).toBe(-1);
    });

    test("requires a replay when moving to an earlier page", () => {
        expect(getInteractiveTerminalAppendIndex(["c", "d"], ["a", "b"])).toBe(-1);
    });

    test("requires a replay when written entries changed", () => {
        expect(getInteractiveTerminalAppendIndex(["a", "b", "c"], ["a", "x", "c", "d"])).toBe(-1);
    });

    test("requires a replay when next is empty", () => {
        expect(getInteractiveTerminalAppendIndex(["a"], [])).toBe(-1);
    });
});

describe("ResponseDisplayInteractive", () => {
    let container;
    let root;

    const encode = (value) => Buffer.from(value, "utf8").toString("base64");
    const settle = async () => {
        for(let i = 0; i < 10; i++){
            await act(async () => {
                jest.advanceTimersByTime(100);
            });
        }
    };
    const streamResponse = async (response) => {
        await act(async () => {
            global.mockApollo.subscription.onData({data: {data: {response_stream: [response]}}});
        });
        await settle();
    };

    beforeAll(() => {
        global.IS_REACT_ACT_ENVIRONMENT = true;
        global.ResizeObserver = class {
            observe(){}
            disconnect(){}
        };
    });

    afterAll(() => {
        delete global.IS_REACT_ACT_ENVIRONMENT;
        delete global.ResizeObserver;
    });

    beforeEach(() => {
        jest.useFakeTimers();
        global.mockApollo.mutations.length = 0;
        global.mockApollo.lazy.getInteractiveLatestResponses?.calls.splice(0);
        container = document.createElement("div");
        document.body.appendChild(container);
        root = createRoot(container);
    });

    afterEach(() => {
        act(() => root.unmount());
        container.remove();
        jest.useRealTimers();
    });

    // the tasking console always renders with selectAllOutput
    test.each([
        ["task view", false, 100],
        ["console view", true, 5000],
    ])("follows streamed output and detects a full-screen app in the %s", async (name, selectAllOutput, windowSize) => {
        const task = {
            id: 1,
            response_count: 0,
            display_id: 1,
            is_interactive_task: false,
            callback: {display_id: 1},
            command: {cmd: "exec_bof"},
        };
        // createRoot is not a Testing Library render helper and needs act to flush effects.
        // eslint-disable-next-line testing-library/no-unnecessary-act
        await act(async () => {
            root.render(<ResponseDisplayInteractive task={task} selectAllOutput={selectAllOutput} />);
        });
        await settle();
        expect(global.mockApollo.lazy.getInteractiveLatestResponses.calls.map((call) => call.variables.fetchLimit))
            .toEqual([windowSize]);
        expect(global.mockApollo.mutations.map((m) => [m.variables.interactive_task_type, m.variables.params]))
            .toEqual([[25, "100,30,0,0"]]);
        expect(global.mockApollo.subscription.skip).toBe(true);

        await act(async () => {
            global.mockApollo.lazy.getInteractiveLatestResponses.options.onCompleted({
                response: [{id: 10, response: encode("$ "), timestamp: "2026-10-15T09:00:00", is_error: false}],
                response_aggregate: {aggregate: {count: 1}},
            });
        });
        await settle();
        expect(global.mockApollo.subscription.skip).toBe(false);
        expect(global.mockApollo.subscription.variables.cursor_id).toBe(10);

        await streamResponse({id: 11, response: encode("\x1b[?1049h\x1b[2J\x1b[1;1Htop"), timestamp: "2026-10-15T09:00:01", is_error: false});
        expect(global.mockTerminal.buffer.active.type).toBe("alternate");
        expect(container.textContent).toContain("Full-screen app");
        expect(container.textContent).toContain("Raw key mode");

        await streamResponse({id: 12, response: encode("\x1b[?1049l$ "), timestamp: "2026-10-15T09:00:02", is_error: false});
        expect(global.mockTerminal.buffer.active.type).toBe("normal");
        expect(container.textContent).not.toContain("Full-screen app");
        expect(container.textContent).toContain("Line mode");
    });
});
