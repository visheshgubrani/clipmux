# Course site

A small course site on the Next.js App Router. An instructor adds lessons;
enrolled students read them. Lessons have no videos yet.

```bash
npm install
npm run dev     # http://localhost:3000
```

There is no real sign-in: the header's "Signed in as" switch sets a cookie.

| User | Role             | May                               |
| ---- | ---------------- | --------------------------------- |
| Ada  | instructor       | add lessons, and see every lesson |
| Ben  | enrolled student | see lessons                       |
| Cy   | not enrolled     | nothing beyond the lesson list    |

The rules are `canTeach` and `canWatch` in `lib/users.ts`, and the signed-in
user is `currentUser()` (server components) or `userFromRequest(req)` (route
handlers). Lessons live in `.data/lessons.json` (`LMS_DATA_DIR` moves it).
