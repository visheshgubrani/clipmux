// What the snippets call that stands for an app's own code: its sessions,
// its access rules and its database. Declared here so `typecheck` compiles
// the snippets against the real @clipmux packages.

interface AppUser {
  id: string
}

declare function requireInstructor(req: Request): Promise<AppUser>
declare function getUser(req: Request): Promise<AppUser>
declare function canWatch(user: AppUser, playbackId: string): Promise<boolean>
declare const db: {
  lesson: {
    update(args: {
      where: { ref: string | null | undefined }
      data: Record<string, unknown>
    }): Promise<unknown>
  }
}
