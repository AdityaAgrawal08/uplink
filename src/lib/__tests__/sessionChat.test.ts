import { describe, it, expect } from "vitest";
import { sanitizeChatText, toMessageDTO } from "../sessionChat";

describe("sanitizeChatText", () => {
  it("accepts ordinary text", () => {
    expect(sanitizeChatText("hello world")).toBe("hello world");
  });

  it("rejects non-strings", () => {
    expect(sanitizeChatText(null)).toBeNull();
    expect(sanitizeChatText(undefined)).toBeNull();
    expect(sanitizeChatText(42)).toBeNull();
    expect(sanitizeChatText({})).toBeNull();
    expect(sanitizeChatText(["hi"])).toBeNull();
  });

  it("rejects empty / whitespace-only", () => {
    expect(sanitizeChatText("")).toBeNull();
    expect(sanitizeChatText("   ")).toBeNull();
  });

  it("strips control characters but keeps tab/newline", () => {
    expect(sanitizeChatText("a\u0000b\u0007c")).toBe("abc");
    expect(sanitizeChatText("keep\ttab\nnl")).toBe("keep\ttab\nnl");
    expect(sanitizeChatText("\u001B[31mred\u001B[0m")).toBe("[31mred[0m"); // ANSI escape stripped
    expect(sanitizeChatText("del\u007Fete")).toBe("delete");
  });

  it("trims surrounding whitespace", () => {
    expect(sanitizeChatText("  padded  ")).toBe("padded");
  });

  it("rejects messages over 500 chars after cleaning (B10: rejects, does not clamp)", () => {
    const ok = "x".repeat(500);
    expect(sanitizeChatText(ok)).toHaveLength(500);
    expect(sanitizeChatText(ok + "y")).toBeNull(); // over-limit rejected, not truncated
  });

  // B18: Newlines are intentionally preserved to support multi-line messages.
  // This test pins that decision so a future "single-line only" change is
  // explicit and deliberate.
  it("preserves newlines for multi-line messages (B18 design decision)", () => {
    expect(sanitizeChatText("line one\nline two")).toBe("line one\nline two");
    expect(sanitizeChatText("a\n\nb")).toBe("a\n\nb");
  });

  it("unicode survives", () => {
    expect(sanitizeChatText("héllo 世界 🚀")).toBe("héllo 世界 🚀");
  });
});

describe("toMessageDTO", () => {
  it("maps a chat doc with ISO date", () => {
    const d = new Date("2026-08-25T10:00:00Z");
    const dto = toMessageDTO({ seq: 3, username: "bob", kind: "chat", text: "hi", createdAt: d });
    expect(dto).toEqual({
      seq: 3, username: "bob", kind: "chat", text: "hi",
      createdAt: d.toISOString(), convId: "general",
    });
  });

  it("derives convId for legacy docs", () => {
    const d = new Date();
    const base = { seq: 0, username: "x", kind: "chat" as const, text: "t", createdAt: d };
    expect(toMessageDTO({ ...base, seq: 1 }).convId).toBe("general");
    expect(toMessageDTO({ ...base, seq: 2, username: "b", to: "a" }).convId).toBe("a|b");
    expect(toMessageDTO({ ...base, seq: 3, username: "system", kind: "system" }).convId).toBe("general");
  });

  it("conversationKey is order-independent", async () => {
    const { conversationKey } = await import("../sessionChat");
    expect(conversationKey("zeta", "alpha")).toBe(conversationKey("alpha", "zeta"));
    expect(conversationKey("zeta", "alpha")).toBe("alpha|zeta");
  });

  it("coerces unknown kinds to chat (fail-visible default)", () => {
    const dto = toMessageDTO({ seq: 1, username: "system", kind: "weird", text: "?", createdAt: new Date() });
    expect(dto.kind).toBe("chat");
  });

  it("preserves system kind", () => {
    const dto = toMessageDTO({ seq: 2, username: "system", kind: "system", text: "alice joined", createdAt: new Date() });
    expect(dto.kind).toBe("system");
  });
});
