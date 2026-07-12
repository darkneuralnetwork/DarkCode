/* 230-tail.js — extracted from app.js (lines 4992-5012) */
async function cancelChatExecution() {
    try {
        await fetch(API + "/api/chat/cancel", { method: "POST" });
        toast("info", "Execution stop requested.");
        if (typeof closeApprovalPopup === "function") {
            closeApprovalPopup("cancelled");
        }
        if (window.currentStreamingMsgEl) {
            window.currentStreamingMsgEl.classList.remove("loading");
            const stopBtn = window.currentStreamingMsgEl.querySelector(".stop-btn");
            if (stopBtn) stopBtn.remove();
        }
    } catch (err) {
        console.error("Failed to cancel", err);
    }
}

let pendingApprovalData = null;

function reopenApprovalModal() {
    if (pendingApprovalData) {
        const overlay = document.getElementById("perm-overlay");
        if (overlay) {
            overlay.style.display = ""; // Clear inline style
        }
        showApprovalPopup(pendingApprovalData);
    }
}
