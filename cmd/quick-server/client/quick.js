// quick.js: server features for sites hosted on quick.
//   import quick from "/__quick/quick.js";
//   const user = await quick.id.me();

export class QuickError extends Error {
  constructor(message, status) {
    super(message);
    this.name = "QuickError";
    this.status = status;
  }
}

async function call(path, { method = "GET", body } = {}) {
  const headers = { "X-Quick": "1" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch("/__quick/api/" + path, {
    method,
    headers,
    credentials: "same-origin",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new QuickError(data?.error || res.statusText, res.status);
  return data;
}

export const id = {
  async me() {
    return (await call("me")).user;
  },
};

export default { id };
