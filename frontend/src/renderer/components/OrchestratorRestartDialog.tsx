import * as Dialog from "@radix-ui/react-dialog";
import { RotateCw, X } from "lucide-react";
import { TopbarButton } from "./TopbarButton";

type OrchestratorRestartDialogProps = {
	open: boolean;
	busy?: boolean;
	onOpenChange: (open: boolean) => void;
	onConfirm: () => void;
};

/** Confirms a deliberate orchestrator restart. Restarting retires the current
 *  orchestrator and spawns a replacement, so the existing session's context is
 *  gone — that is the point when starting an unrelated stream of work, but it is
 *  not recoverable, which is why this is behind a confirmation. */
export function OrchestratorRestartDialog({ open, busy, onOpenChange, onConfirm }: OrchestratorRestartDialogProps) {
	return (
		<Dialog.Root open={open} onOpenChange={onOpenChange}>
			<Dialog.Portal>
				<Dialog.Overlay className="fixed inset-0 z-overlay bg-scrim" />
				<Dialog.Content className="fixed left-1/2 top-1/2 z-overlay w-dialog-orchestrator -translate-x-1/2 -translate-y-1/2 rounded-lg border border-border bg-surface p-5 shadow-lg">
					<div className="flex items-start gap-3">
						<div className="grid size-8 shrink-0 place-items-center rounded-md border border-border bg-muted text-muted-foreground">
							<RotateCw className="size-icon-base" aria-hidden="true" />
						</div>
						<div className="min-w-0 flex-1">
							<Dialog.Title className="text-sm font-medium text-foreground">Restart orchestrator?</Dialog.Title>
							<Dialog.Description className="mt-2 text-[13px] leading-5 text-muted-foreground">
								The current orchestrator is retired and a fresh one takes over on the canonical branch. Its conversation
								context is discarded and cannot be recovered. Running worker sessions are not stopped.
							</Dialog.Description>
						</div>
						<Dialog.Close asChild>
							<button
								className="rounded-md p-1 text-passive hover:bg-interactive-hover hover:text-foreground"
								type="button"
							>
								<X className="size-icon-base" aria-hidden="true" />
								<span className="sr-only">Close</span>
							</button>
						</Dialog.Close>
					</div>
					<div className="mt-5 flex justify-end gap-2">
						<TopbarButton onClick={() => onOpenChange(false)} variant="killCancel">
							Cancel
						</TopbarButton>
						<TopbarButton disabled={busy} onClick={onConfirm} variant="primary">
							<RotateCw className="size-3.5" aria-hidden="true" />
							{busy ? "Restarting..." : "Restart"}
						</TopbarButton>
					</div>
				</Dialog.Content>
			</Dialog.Portal>
		</Dialog.Root>
	);
}
