#include <iostream>
#include <string_view>

int main(int argc, char* argv[]) {
    if (argc == 2 && std::string_view(argv[1]) == "--version") {
        std::cout << "envelopefile 0.1.0\n";
        return 0;
    }
    std::cerr << "Usage: envelopefile --version\n";
    return 2;
}
