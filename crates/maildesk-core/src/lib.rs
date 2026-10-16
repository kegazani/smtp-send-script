//! Portable MailDesk engine. No web server, browser, or UI dependencies.
mod model;
mod store;
mod transport;
pub use model::*;
pub use store::*;
pub use transport::*;
