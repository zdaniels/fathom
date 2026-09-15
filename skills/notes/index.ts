export interface Note {
  id: string;
  title: string;
  content: string;
  tags: string[];
  createdAt: string;
  updatedAt: string;
}

const store = new Map<string, Note>();

export async function createNote(params: {
  title: string;
  content: string;
  tags?: string[];
}): Promise<{ id: string }> {
  const id = crypto.randomUUID();
  const now = new Date().toISOString();
  const note: Note = {
    id,
    title: params.title,
    content: params.content,
    tags: params.tags ?? [],
    createdAt: now,
    updatedAt: now,
  };
  store.set(id, note);
  return { id };
}

export async function getNote(params: { id: string }): Promise<Note | null> {
  return store.get(params.id) ?? null;
}

export async function updateNote(params: {
  id: string;
  title?: string;
  content?: string;
  tags?: string[];
}): Promise<{ ok: boolean }> {
  const note = store.get(params.id);
  if (!note) return { ok: false };
  if (params.title != null) note.title = params.title;
  if (params.content != null) note.content = params.content;
  if (params.tags != null) note.tags = params.tags;
  note.updatedAt = new Date().toISOString();
  store.set(params.id, note);
  return { ok: true };
}

export async function deleteNote(params: { id: string }): Promise<{ ok: boolean }> {
  return { ok: store.delete(params.id) };
}

export async function searchNotes(params: {
  query: string;
}): Promise<{ notes: Note[] }> {
  const q = params.query.toLowerCase();
  const notes: Note[] = [];
  for (const note of store.values()) {
    if (
      note.title.toLowerCase().includes(q) ||
      note.content.toLowerCase().includes(q)
    ) {
      notes.push(note);
    }
  }
  return { notes };
}

export async function listNotes(params: {
  tag?: string;
  limit?: number;
}): Promise<{ notes: Note[] }> {
  let notes = Array.from(store.values());
  if (params.tag) {
    const tag = params.tag.toLowerCase();
    notes = notes.filter((n) =>
      n.tags.some((t) => t.toLowerCase() === tag)
    );
  }
  notes.sort((a, b) => new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime());
  const limit = params.limit ?? 50;
  return { notes: notes.slice(0, limit) };
}

export const tools = [
  {
    name: "create_note",
    description: "Create a new note",
    parameters: {
      type: "object",
      properties: {
        title: { type: "string", description: "Note title" },
        content: { type: "string", description: "Note content" },
        tags: { type: "array", items: { type: "string" }, description: "Tags" },
      },
      required: ["title", "content"],
    },
  },
  {
    name: "get_note",
    description: "Get a note by ID",
    parameters: {
      type: "object",
      properties: { id: { type: "string", description: "Note ID" } },
      required: ["id"],
    },
  },
  {
    name: "update_note",
    description: "Update an existing note",
    parameters: {
      type: "object",
      properties: {
        id: { type: "string", description: "Note ID" },
        title: { type: "string", description: "New title" },
        content: { type: "string", description: "New content" },
        tags: { type: "array", items: { type: "string" }, description: "New tags" },
      },
      required: ["id"],
    },
  },
  {
    name: "delete_note",
    description: "Delete a note",
    parameters: {
      type: "object",
      properties: { id: { type: "string", description: "Note ID" } },
      required: ["id"],
    },
  },
  {
    name: "search_notes",
    description: "Search notes by full-text query",
    parameters: {
      type: "object",
      properties: { query: { type: "string", description: "Search query" } },
      required: ["query"],
    },
  },
  {
    name: "list_notes",
    description: "List notes, optionally filtered by tag",
    parameters: {
      type: "object",
      properties: {
        tag: { type: "string", description: "Filter by tag" },
        limit: { type: "number", description: "Maximum number of notes" },
      },
      required: [],
    },
  },
];
