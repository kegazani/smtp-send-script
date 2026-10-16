fn main() {
    #[cfg(windows)]
    {
        winresource::WindowsResource::new()
            .set_icon("../../assets/maildesk.ico")
            .set("ProductName", "MailDesk")
            .set("FileDescription", "MailDesk — email workspace")
            .compile()
            .expect("compile Windows icon");
    }
}
