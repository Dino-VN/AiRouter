import { clsx, type ClassValue } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

/**
 * Mirrors model.ValidUsername in the Go server: 3-32 ASCII letters, digits, dot,
 * underscore or hyphen, starting and ending with a letter or digit. The server
 * checks this too — this copy only exists so a form can say so before the round
 * trip, and the wording shown to the user comes from meta.username_rule so the
 * two can never contradict each other.
 */
export function validUsername(name: string): boolean {
  return /^[A-Za-z0-9][A-Za-z0-9._-]{1,30}[A-Za-z0-9]$/.test(name)
}

